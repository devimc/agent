//
// Copyright (c) 2017 Intel Corporation
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	hyper "github.com/clearcontainers/agent/api"
	"github.com/opencontainers/runc/libcontainer"
	"github.com/opencontainers/runc/libcontainer/configs"
	_ "github.com/opencontainers/runc/libcontainer/nsenter"
	"github.com/opencontainers/runc/libcontainer/utils"
	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

const (
	virtIOPath         = "/sys/class/virtio-ports"
	devRootPath        = "/dev"
	ctlChannelName     = "sh.hyper.channel.0"
	ttyChannelName     = "sh.hyper.channel.1"
	ctlHeaderSize      = 8
	ttyHeaderSize      = 12
	mountShareDirDest  = "/tmp/shareDir"
	type9pFs           = "9p"
	containerMountDest = "/tmp/hyper"
	loName             = "lo"
	loIPAddr           = "127.0.0.1"
	loNetMask          = "0"
	loType             = "loopback"
	loGateway          = "localhost"
	defaultPassword    = "/etc/passwd"
	statelessPassword  = "/usr/share/defaults/etc/passwd"
	defaultGroup       = "/etc/group"
	statelessGroup     = "/usr/share/defaults/etc/group"
	kernelCmdlineFile  = "/proc/cmdline"
	optionPrefix       = "agent."
	logLevelFlag       = optionPrefix + "log"
	defaultLogLevel    = logrus.InfoLevel
	cannotGetTimeMsg   = "Failed to get time for event %s:%v"
)

var capsList = []string{
	"CAP_AUDIT_CONTROL",
	"CAP_AUDIT_READ",
	"CAP_AUDIT_WRITE",
	"CAP_BLOCK_SUSPEND",
	"CAP_CHOWN",
	"CAP_DAC_OVERRIDE",
	"CAP_DAC_READ_SEARCH",
	"CAP_FOWNER",
	"CAP_FSETID",
	"CAP_IPC_LOCK",
	"CAP_IPC_OWNER",
	"CAP_KILL",
	"CAP_LEASE",
	"CAP_LINUX_IMMUTABLE",
	"CAP_MAC_ADMIN",
	"CAP_MAC_OVERRIDE",
	"CAP_MKNOD",
	"CAP_NET_ADMIN",
	"CAP_NET_BIND_SERVICE",
	"CAP_NET_BROADCAST",
	"CAP_NET_RAW",
	"CAP_SETGID",
	"CAP_SETFCAP",
	"CAP_SETPCAP",
	"CAP_SETUID",
	"CAP_SYS_ADMIN",
	"CAP_SYS_BOOT",
	"CAP_SYS_CHROOT",
	"CAP_SYS_MODULE",
	"CAP_SYS_NICE",
	"CAP_SYS_PACCT",
	"CAP_SYS_PTRACE",
	"CAP_SYS_RAWIO",
	"CAP_SYS_RESOURCE",
	"CAP_SYS_TIME",
	"CAP_SYS_TTY_CONFIG",
	"CAP_SYSLOG",
	"CAP_WAKE_ALARM",
}

type agentConfig struct {
	logLevel logrus.Level
}

type process struct {
	process     libcontainer.Process
	stdin       *os.File
	stdout      *os.File
	stderr      *os.File
	seqStdio    uint64
	seqStderr   uint64
	consoleSock *os.File
	termMaster  *os.File
}

type container struct {
	container     libcontainer.Container
	config        configs.Config
	processes     map[string]*process
	pod           *pod
	processesLock sync.RWMutex
	wgProcesses   sync.WaitGroup
}

type pod struct {
	id             string
	running        bool
	containers     map[string]*container
	ctl            *os.File
	tty            *os.File
	stdinList      map[uint64]*os.File
	network        hyper.Network
	stdinLock      sync.Mutex
	ttyLock        sync.Mutex
	containersLock sync.RWMutex
}

type cmdCb func(*pod, []byte) error

var callbackList = map[hyper.HyperCmd]cmdCb{
	hyper.StartPodCmd:        startPodCb,
	hyper.DestroyPodCmd:      destroyPodCb,
	hyper.NewContainerCmd:    newContainerCb,
	hyper.KillContainerCmd:   killContainerCb,
	hyper.RemoveContainerCmd: removeContainerCb,
	hyper.ExecCmd:            execCb,
	hyper.ReadyCmd:           readyCb,
	hyper.PingCmd:            pingCb,
	hyper.WinsizeCmd:         winsizeCb,
}

var agentLog = logrus.New()

func init() {
	if len(os.Args) > 1 && os.Args[1] == "init" {
		runtime.GOMAXPROCS(1)
		runtime.LockOSThread()
		factory, _ := libcontainer.New("")
		if err := factory.StartInitialization(); err != nil {
			agentLog.Errorf("init went wrong: %v", err)
		}
		panic("--this line should have never been executed, congratulations--")
	}
}

// Version is the agent version. This variable is populated at build time.
var Version = "unknown"

func main() {

	agentLog.Formatter = &logrus.JSONFormatter{TimestampFormat: time.RFC3339Nano}
	config := newConfig(defaultLogLevel)
	if err := config.getConfig(kernelCmdlineFile); err != nil {
		agentLog.Warnf("Failed to get config from ernel cmdline: %v", err)
	}
	applyConfig(config)

	agentLog.Infof("Agent version: %s", Version)

	if uptime, err := newEventTime(agentStartedEvent); err != nil {
		agentLog.Errorf("Failed to get uptime %v", err)
	} else {
		agentLog.Infof("%s", uptime)
	}

	// Initialiaze wait group waiting for loops to be terminated
	var wgLoops sync.WaitGroup
	wgLoops.Add(1)

	// Initialize unique pod structure
	pod := &pod{
		containers: make(map[string]*container),
		running:    false,
		stdinList:  make(map[uint64]*os.File),
	}

	// Open serial ports and write on both CTL and TTY channels
	if err := pod.openChannels(); err != nil {
		agentLog.Errorf("Could not open channels: %v", err)
		return
	}
	defer pod.closeChannels()

	// Setup users and groups
	if err := pod.setupUsersAndGroups(); err != nil {
		agentLog.Errorf("Could not setup users and groups: %v", err)
		return
	}

	// Run CTL loop
	go pod.controlLoop(&wgLoops)

	// Run TTY loop
	go pod.streamsLoop(&wgLoops)

	wgLoops.Wait()
}

func (p *pod) getContainer(id string) (ctr *container) {
	p.containersLock.RLock()
	defer p.containersLock.RUnlock()

	ctr, exist := p.containers[id]
	if exist == false {
		return nil
	}

	return ctr
}

func (p *pod) setContainer(id string, ctr *container) {
	p.containersLock.Lock()
	p.containers[id] = ctr
	p.containersLock.Unlock()
}

func (p *pod) deleteContainer(id string) {
	p.containersLock.Lock()
	delete(p.containers, id)
	p.containersLock.Unlock()
}

func (p *pod) controlLoop(wg *sync.WaitGroup) {
	// Send READY right after it has connected.
	// This allows to block until the connection is up.
	if err := p.sendCmd(hyper.ReadyCmd, []byte{}); err != nil {
		agentLog.Errorf("Could not send 'ready' command: %v", err)
		goto out
	}

	for {
		reply := hyper.AckCmd
		cmd, data, err := p.readCtl()
		if err != nil {
			if err == io.EOF {
				time.Sleep(time.Millisecond)
				continue
			}

			agentLog.Errorf("Read on ctl channel failed: %v", err)
			break
		}

		agentLog.Infof("Received %q command", hyper.CmdToString(cmd))

		if err := p.runCmd(cmd, data); err != nil {
			agentLog.Errorf("Run %q command failed: %v", hyper.CmdToString(cmd), err)
			reply = hyper.ErrorCmd
		}

		if err := p.sendCmd(reply, []byte{}); err != nil {
			agentLog.Errorf("Send reply on ctl channel failed: %v", err)
			break
		}

		if cmd == hyper.DestroyPodCmd {
			break
		}
	}

out:
	wg.Done()
}

func (p *pod) streamsLoop(wg *sync.WaitGroup) {
	// Wait for reading something on TTY
	for {
		seq, data, err := p.readTty()
		if err != nil {
			if err == io.EOF {
				time.Sleep(time.Millisecond)
				continue
			}

			agentLog.Infof("Read on tty channel failed: %v", err)
			break
		}

		agentLog.Infof("Read from TTY\n")

		if seq == uint64(0) || data == nil {
			continue
		}

		agentLog.Infof("Read from tty: sequence %d / message %s", seq, string(data))

		// Lock the list before we access it.
		p.stdinLock.Lock()

		file, exist := p.stdinList[seq]
		if !exist {
			agentLog.Infof("Sequence %d not found for stdin", seq)
			p.stdinLock.Unlock()
			continue
		}

		p.stdinLock.Unlock()

		agentLog.Infof("Sequence %d found, writing data to stdin", seq)

		n, err := file.Write(data)
		if err != nil {
			agentLog.Errorf("Write to process stdin failed: %v", err)
		}

		agentLog.Infof("%d bytes written out of %d", n, len(data))
	}

	wg.Done()
}

func (p *pod) registerStdin(seq uint64, stdin *os.File) error {
	p.stdinLock.Lock()
	defer p.stdinLock.Unlock()

	if _, exist := p.stdinList[seq]; exist {
		return fmt.Errorf("Sequence number %d already registered", seq)
	}

	p.stdinList[seq] = stdin

	return nil
}

func (p *pod) unregisterStdin(seq uint64) {
	p.stdinLock.Lock()
	defer p.stdinLock.Unlock()

	delete(p.stdinList, seq)
}

func (p *pod) openChannels() error {
	ctlPath, err := findVirtualSerialPath(ctlChannelName)
	if err != nil {
		return err
	}

	ttyPath, err := findVirtualSerialPath(ttyChannelName)
	if err != nil {
		return err
	}

	ctl, err := os.OpenFile(ctlPath, os.O_RDWR, os.ModeDevice)
	if err != nil {
		return err
	}

	tty, err := os.OpenFile(ttyPath, os.O_RDWR, os.ModeDevice)
	if err != nil {
		ctl.Close()
		return err
	}

	p.ctl = ctl
	p.tty = tty

	return nil
}

func (p *pod) closeChannels() {
	if p.ctl != nil {
		p.ctl.Close()
		p.ctl = nil
	}

	if p.tty != nil {
		p.tty.Close()
		p.tty = nil
	}
}

func (p *pod) setupUsersAndGroups() error {

	// Check /etc/passwd for users
	if _, err := os.Stat(defaultPassword); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		if _, err := os.Stat(statelessPassword); err != nil {
			return err
		}

		if err := fileCopy(statelessPassword, defaultPassword); err != nil {
			return err
		}
	}

	// Check /etc/group for groups
	if _, err := os.Stat(defaultGroup); err != nil {
		if !os.IsNotExist(err) {
			return err
		}

		if _, err := os.Stat(statelessGroup); err != nil {
			return err
		}

		if err := fileCopy(statelessGroup, defaultGroup); err != nil {
			return err
		}
	}

	return nil
}

func findVirtualSerialPath(serialName string) (string, error) {
	dir, err := os.Open(virtIOPath)
	if err != nil {
		return "", err
	}

	defer dir.Close()

	ports, err := dir.Readdirnames(0)
	if err != nil {
		return "", err
	}

	for _, port := range ports {
		path := filepath.Join(virtIOPath, port, "name")
		if _, err := os.Stat(path); err != nil {
			if os.IsNotExist(err) {
				agentLog.Debugf("Skip parsing of %s as it does not exist", path)
				continue
			}

			return "", err
		}

		content, err := ioutil.ReadFile(path)
		if err != nil {
			return "", err
		}

		if strings.Contains(string(content), serialName) == true {
			return filepath.Join(devRootPath, port), nil
		}
	}

	return "", fmt.Errorf("Could not find virtio port %s", serialName)
}

func (p *pod) readCtl() (hyper.HyperCmd, []byte, error) {
	buf := make([]byte, ctlHeaderSize)

	n, err := p.ctl.Read(buf)
	if err != nil {
		return hyper.ErrorCmd, []byte{}, err
	}

	if n != ctlHeaderSize {
		return hyper.ErrorCmd, []byte{},
			fmt.Errorf("Only %d bytes read out of %d expected (ctl channel)", n, ctlHeaderSize)
	}

	cmd := hyper.HyperCmd(binary.BigEndian.Uint32(buf[:4]))
	length := int(binary.BigEndian.Uint32(buf[4:ctlHeaderSize]))
	length -= ctlHeaderSize
	if length == 0 {
		return cmd, nil, nil
	}

	data := make([]byte, length)

	n, err = p.ctl.Read(data)
	if err != nil {
		return hyper.ErrorCmd, []byte{}, err
	}

	if n != length {
		return hyper.ErrorCmd, []byte{},
			fmt.Errorf("Only %d bytes read out of %d expected (ctl channel)", n, length)
	}

	return cmd, data, nil
}

func (p *pod) readTty() (uint64, []byte, error) {
	buf := make([]byte, ttyHeaderSize)

	n, err := p.tty.Read(buf)
	if err != nil {
		return uint64(0), []byte{}, err
	}

	if n != ttyHeaderSize {
		return uint64(0), []byte{},
			fmt.Errorf("Only %d bytes read out of %d expected (tty channel)", n, ttyHeaderSize)
	}

	seq := binary.BigEndian.Uint64(buf[:8])
	length := int(binary.BigEndian.Uint32(buf[8:ttyHeaderSize]))
	length -= ttyHeaderSize
	if length == 0 {
		return seq, nil, nil
	}

	data := make([]byte, length)

	n, err = p.tty.Read(data)
	if err != nil {
		return uint64(0), []byte{}, err
	}

	if n != length {
		return uint64(0), []byte{},
			fmt.Errorf("Only %d bytes read out of %d expected (tty channel)", n, length)
	}

	return seq, data, nil
}

func (p *pod) sendCmd(cmd hyper.HyperCmd, data []byte) error {
	dataLen := len(data)
	length := ctlHeaderSize + dataLen
	buf := make([]byte, length)

	binary.BigEndian.PutUint32(buf[:], uint32(cmd))
	binary.BigEndian.PutUint32(buf[4:], uint32(length))

	if dataLen > 0 {
		bytesCopied := copy(buf[ctlHeaderSize:], data)
		if bytesCopied != dataLen {
			return fmt.Errorf("Only %d bytes copied out of %d expected (ctl channel)", bytesCopied, dataLen)
		}
	}

	n, err := p.ctl.Write(buf)
	if err != nil {
		return err
	}

	if n != length {
		return fmt.Errorf("Only %d bytes written out of %d expected (ctl channel)", n, length)
	}

	return nil
}

func (p *pod) sendSeq(seq uint64, data []byte) error {
	p.ttyLock.Lock()
	defer p.ttyLock.Unlock()

	dataLen := len(data)
	length := ttyHeaderSize + dataLen
	buf := make([]byte, length)

	binary.BigEndian.PutUint64(buf[:], seq)
	binary.BigEndian.PutUint32(buf[8:], uint32(length))

	if dataLen > 0 {
		bytesCopied := copy(buf[ttyHeaderSize:], data)
		if bytesCopied != dataLen {
			return fmt.Errorf("Only %d bytes copied out of %d expected (tty channel)", bytesCopied, dataLen)
		}
	}

	n, err := p.tty.Write(buf)
	if err != nil {
		return err
	}

	if n != length {
		return fmt.Errorf("Only %d bytes written out of %d expected (tty channel)", n, length)
	}

	return nil
}

func (c *container) getProcess(pid string) *process {
	c.processesLock.RLock()
	defer c.processesLock.RUnlock()

	proc, exist := c.processes[pid]
	if exist == false {
		return nil
	}

	return proc
}

func (c *container) setProcess(pid string, process *process) {
	c.processesLock.Lock()
	c.processes[pid] = process
	c.processesLock.Unlock()
}

func (c *container) deleteProcess(pid string) {
	c.processesLock.Lock()
	delete(c.processes, pid)
	c.processesLock.Unlock()
}

func (c *container) closeProcessStreams(pid string) {
	cid := c.container.ID()
	proc := c.getProcess(pid)

	if proc == nil {
		agentLog.Warnf("Process %s for container %s does not exist anymore", pid, cid)
		return
	}

	if proc.termMaster != nil {
		if err := proc.termMaster.Close(); err != nil {
			agentLog.Warnf("Could not close stderr for container %s, process %s: %v", cid, pid, err)
		}

		proc.termMaster = nil
	}

	if proc.stdout != nil {
		if err := proc.stdout.Close(); err != nil {
			agentLog.Warnf("Could not close stdout for container %s, process %s: %v", cid, pid, err)
		}

		proc.stdout = nil
	}

	if proc.stderr != nil {
		if err := proc.stderr.Close(); err != nil {
			agentLog.Warnf("Could not close stderr for container %s, process %s: %v", cid, pid, err)
		}

		proc.stderr = nil
	}

	c.pod.unregisterStdin(c.processes[pid].seqStdio)

	if proc.stdin != nil {
		if err := proc.stdin.Close(); err != nil {
			agentLog.Warnf("Could not close stdin for container %s, process %s: %v", cid, pid, err)
		}

		proc.stdin = nil
	}
}

func (c *container) closeProcessPipes(pid string) {
	cid := c.container.ID()
	proc := c.getProcess(pid)

	if proc == nil {
		agentLog.Warnf("Process %s for container %s does not exist anymore", pid, cid)
		return
	}

	if proc.process.Stdout != nil {
		if err := proc.process.Stdout.(*os.File).Close(); err != nil {
			agentLog.Warnf("Could not close process.Stdout for container %s, process %s: %v", cid, pid, err)
		}

		proc.process.Stdout = nil
	}

	if proc.process.Stderr != nil {
		if err := proc.process.Stderr.(*os.File).Close(); err != nil {
			agentLog.Warnf("Could not close process.Stderr for container %s, process %s: %v", cid, pid, err)
		}

		proc.process.Stderr = nil
	}

	if proc.process.Stdin != nil {
		if err := proc.process.Stdin.(*os.File).Close(); err != nil {
			agentLog.Warnf("Could not close process.Stdin for container %s, process %s: %v", cid, pid, err)
		}

		proc.process.Stdin = nil
	}
}

// Executed as a go routine to route stdout and stderr to the TTY channel.
func (p *pod) routeOutput(seq uint64, stream *os.File, wg *sync.WaitGroup) {
	for {
		buf := make([]byte, 1024)

		n, err := stream.Read(buf)
		if err != nil {
			agentLog.Infof("Stream %d has been closed: %v", seq, err)
			break
		}

		agentLog.Infof("Read from stream seq %d: %q", seq, string(buf[:n]))
		p.sendSeq(seq, buf[:n])
	}

	wg.Done()
}

func setConsoleCarriageReturn(fd uintptr) error {
	var termios syscall.Termios

	if err := ioctl(fd, syscall.TCGETS, uintptr(unsafe.Pointer(&termios))); err != nil {
		return err
	}

	termios.Oflag |= syscall.ONLCR

	if err := ioctl(fd, syscall.TCSETS, uintptr(unsafe.Pointer(&termios))); err != nil {
		return err
	}

	return nil
}

// Executed as go routine to run and wait for the process.
func (p *pod) runContainerProcess(cid, pid string, terminal bool, started chan error) error {
	ctr := p.getContainer(cid)

	defer func() {
		ctr.wgProcesses.Done()
		ctr.deleteProcess(pid)
		ctr.closeProcessStreams(pid)
		ctr.closeProcessPipes(pid)
	}()

	var wgRouteOutput sync.WaitGroup

	proc := ctr.getProcess(pid)

	if err := ctr.container.Run(&(proc.process)); err != nil {
		agentLog.Errorf("Could not run process %s: %v", pid, err)
		started <- err
		return err
	}

	if terminal {
		termMaster, err := utils.RecvFd(proc.consoleSock)
		if err != nil {
			return err
		}

		if err := setConsoleCarriageReturn(termMaster.Fd()); err != nil {
			return err
		}

		proc.termMaster = termMaster

		if err := p.registerStdin(proc.seqStdio, termMaster); err != nil {
			return err
		}

		wgRouteOutput.Add(1)
		go p.routeOutput(proc.seqStdio, termMaster, &wgRouteOutput)
	} else {
		if proc.stdout != nil {
			wgRouteOutput.Add(1)
			go p.routeOutput(proc.seqStdio,
				proc.stdout, &wgRouteOutput)
		}

		if proc.stderr != nil {
			wgRouteOutput.Add(1)
			go p.routeOutput(proc.seqStderr,
				proc.stderr, &wgRouteOutput)
		}
	}

	started <- nil

	processState, err := proc.process.Wait()
	if err != nil {
		agentLog.Errorf("Wait for process %s failed: %v", pid, err)
	}

	// Close pipes to terminate routeOutput() go routines.
	ctr.closeProcessPipes(pid)

	// Wait for routeOutput() go routines.
	wgRouteOutput.Wait()

	// Send empty message on tty channel to close the IO stream
	p.sendSeq(proc.seqStdio, []byte{})

	// Get exit code
	exitCode := uint8(255)
	if processState != nil {
		agentLog.Infof("ProcessState: %+v", processState)
		if waitStatus, ok := processState.Sys().(syscall.WaitStatus); ok {
			exitCode = uint8(waitStatus.ExitStatus())
			agentLog.Infof("Exit Code after Wait: %+v", exitCode)
		}
	}

	// Send exit code through tty channel
	p.sendSeq(proc.seqStdio, []byte{exitCode})

	return nil
}

func (p *pod) buildProcessWithoutTerminal(proc *process) (*process, error) {
	rStdin, wStdin, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	rStdout, wStdout, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	rStderr, wStderr, err := os.Pipe()
	if err != nil {
		return nil, err
	}

	if err := p.registerStdin(proc.seqStdio, wStdin); err != nil {
		return nil, err
	}

	proc.process.Stdin = rStdin
	proc.process.Stdout = wStdout
	proc.process.Stderr = wStderr

	proc.stdin = wStdin
	proc.stdout = rStdout
	proc.stderr = rStderr

	return proc, nil
}

func (p *pod) buildProcessWithTerminal(proc *process) (*process, error) {
	parentSock, childSock, err := utils.NewSockPair("console")
	if err != nil {
		return nil, err
	}

	proc.process.ConsoleSocket = childSock

	proc.consoleSock = parentSock

	return proc, nil
}

func (p *pod) buildProcess(hyperProcess hyper.Process) (*process, error) {
	var envList []string
	for _, env := range hyperProcess.Envs {
		envList = append(envList, fmt.Sprintf("%s=%s", env.Env, env.Value))
	}

	// we can specify the user and the group separated by :
	user := fmt.Sprintf("%s:%s", hyperProcess.User, hyperProcess.Group)

	libContProcess := libcontainer.Process{
		Cwd:              hyperProcess.Workdir,
		Args:             hyperProcess.Args,
		Env:              envList,
		User:             user,
		AdditionalGroups: hyperProcess.AdditionalGroups,
	}

	proc := &process{
		process:   libContProcess,
		seqStdio:  hyperProcess.Stdio,
		seqStderr: hyperProcess.Stderr,
	}

	if hyperProcess.Terminal {
		return p.buildProcessWithTerminal(proc)
	}

	return p.buildProcessWithoutTerminal(proc)
}

func (p *pod) runCmd(cmd hyper.HyperCmd, data []byte) error {
	cb, exist := callbackList[cmd]
	if exist == false {
		return fmt.Errorf("No callback found for command %q", hyper.CmdToString(cmd))
	}

	agentLog.Infof("%s", hyper.CmdToString(cmd)+"_start")

	cbErr := cb(p, data)

	agentLog.Infof("%s", hyper.CmdToString(cmd)+"_end")
	return cbErr
}

func startPodCb(pod *pod, data []byte) error {
	var payload hyper.StartPod

	if pod.running == true {
		return fmt.Errorf("Pod already started, impossible to start again")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	if err := mountShareDir(payload.ShareDir); err != nil {
		return err
	}

	pod.id = payload.ID
	pod.running = true
	pod.network = hyper.Network{
		Interfaces: payload.Interfaces,
		DNS:        payload.DNS,
		Routes:     payload.Routes,
	}

	if err := pod.setupNetwork(); err != nil {
		return fmt.Errorf("Could not setup the network: %v", err)
	}

	return nil
}

func destroyPodCb(pod *pod, data []byte) error {
	if pod.running == false {
		agentLog.Infof("Pod not started, this is a no-op")
		return nil
	}

	pod.containersLock.Lock()
	for key, c := range pod.containers {
		if err := c.removeContainer(key); err != nil {
			return err
		}

		delete(pod.containers, key)
	}
	pod.containersLock.Unlock()

	if err := pod.removeNetwork(); err != nil {
		return fmt.Errorf("Could not remove the network: %v", err)
	}

	if err := unmountShareDir(); err != nil {
		return err
	}

	pod.id = ""
	pod.containers = make(map[string]*container)
	pod.running = false
	pod.stdinList = make(map[uint64]*os.File)
	pod.network = hyper.Network{}

	return nil
}

func addMounts(config *configs.Config, fsmaps []hyper.Fsmap) error {
	for _, fsmap := range fsmaps {
		newMount := &configs.Mount{
			Source:      filepath.Join(mountShareDirDest, fsmap.Source),
			Destination: fsmap.Path,
			Device:      "bind",
			Flags:       syscall.MS_BIND | syscall.MS_REC,
		}

		if fsmap.ReadOnly {
			newMount.Flags |= syscall.MS_RDONLY
		}

		config.Mounts = append(config.Mounts, newMount)
	}

	return nil
}

func newContainerCb(pod *pod, data []byte) error {
	var payload hyper.NewContainer

	if pod.running == false {
		return fmt.Errorf("Pod not started, impossible to run a new container")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	if payload.Process.ID == "" {
		payload.Process.ID = fmt.Sprintf("%d", payload.Process.Stdio)
	}

	if ctr := pod.getContainer(payload.ID); ctr != nil {
		return fmt.Errorf("Container %s already exists, impossible to create", payload.ID)
	}

	absoluteRootFs, err := mountContainerRootFs(payload.ID, payload.Image, payload.RootFs, payload.FsType)
	if err != nil {
		return err
	}

	defaultMountFlags := syscall.MS_NOEXEC | syscall.MS_NOSUID | syscall.MS_NODEV

	config := configs.Config{
		Rootfs: absoluteRootFs,
		Capabilities: &configs.Capabilities{
			Bounding:    capsList,
			Effective:   capsList,
			Inheritable: capsList,
			Permitted:   capsList,
			Ambient:     capsList,
		},
		Namespaces: configs.Namespaces([]configs.Namespace{
			{Type: configs.NEWNS},
			{Type: configs.NEWUTS},
			{Type: configs.NEWIPC},
			{Type: configs.NEWPID},
		}),
		Cgroups: &configs.Cgroup{
			Name:   payload.ID,
			Parent: "system",
			Resources: &configs.Resources{
				MemorySwappiness: nil,
				AllowAllDevices:  nil,
				AllowedDevices:   configs.DefaultAllowedDevices,
			},
		},
		Devices: configs.DefaultAutoCreatedDevices,

		Hostname: pod.id,
		Mounts: []*configs.Mount{
			{
				Source:      "proc",
				Destination: "/proc",
				Device:      "proc",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "tmpfs",
				Destination: "/dev",
				Device:      "tmpfs",
				Flags:       syscall.MS_NOSUID | syscall.MS_STRICTATIME,
				Data:        "mode=755",
			},
			{
				Source:      "devpts",
				Destination: "/dev/pts",
				Device:      "devpts",
				Flags:       syscall.MS_NOSUID | syscall.MS_NOEXEC,
				Data:        "newinstance,ptmxmode=0666,mode=0620,gid=5",
			},
			{
				Device:      "tmpfs",
				Source:      "shm",
				Destination: "/dev/shm",
				Data:        "mode=1777,size=65536k",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "mqueue",
				Destination: "/dev/mqueue",
				Device:      "mqueue",
				Flags:       defaultMountFlags,
			},
			{
				Source:      "sysfs",
				Destination: "/sys",
				Device:      "sysfs",
				Flags:       defaultMountFlags | unix.MS_RDONLY,
			},
		},

		NoNewKeyring: true,
	}

	// Populate config.Mounts with additional mounts provided through
	// fsmap.
	if err := addMounts(&config, payload.Fsmap); err != nil {
		return err
	}

	containerPath := filepath.Join("/tmp/libcontainer", pod.id)
	factory, err := libcontainer.New(containerPath, libcontainer.Cgroupfs)
	if err != nil {
		return err
	}

	libContContainer, err := factory.Create(payload.ID, &config)
	if err != nil {
		return err
	}

	builtProcess, err := pod.buildProcess(payload.Process)
	if err != nil {
		return err
	}

	processes := make(map[string]*process)
	processes[payload.Process.ID] = builtProcess

	container := &container{
		pod:       pod,
		container: libContContainer,
		config:    config,
		processes: processes,
	}

	pod.setContainer(payload.ID, container)

	container.wgProcesses.Add(1)

	started := make(chan error)
	go pod.runContainerProcess(payload.ID, payload.Process.ID, payload.Process.Terminal, started)

	select {
	case err := <-started:
		if err != nil {
			return fmt.Errorf("Process could not be started: %v", err)
		}
	case <-time.After(time.Duration(5) * time.Second):
		return fmt.Errorf("Process could not be started: timeout error")
	}

	return nil
}

func waitForContainerWorkload(c libcontainer.Container) {
	sigCgtTag := "SigCgt:"
	maxTries := 5

	pids, err := c.Processes()
	if err != nil {
		agentLog.Errorf("wait for container workload unable to get container processes: %s", err)
		return
	}

	if len(pids) == 0 {
		return
	}

	getSigCgt := func() (string, error) {
		statusPath := fmt.Sprintf("/proc/%d/status", pids[0])
		statusContent, err := ioutil.ReadFile(statusPath)
		if err != nil {
			return "", fmt.Errorf("unable to read %s: %s", statusPath, err)
		}

		status := string(statusContent)

		begin := strings.Index(status, sigCgtTag)
		if begin == -1 {
			return "", fmt.Errorf("unable to find tag %s in %s", sigCgtTag, status)
		}

		begin += len(sigCgtTag)
		end := strings.Index(status[begin:], "\n")

		return strings.TrimSpace(status[begin : begin+end]), nil
	}

	for i := 0; i < maxTries; i++ {
		signalsCgt, err := getSigCgt()
		if err != nil {
			agentLog.Errorf("failed to wait for container workload: %s", err)
			return
		}

		for _, c := range signalsCgt {
			if string(c) != "0" {
				return
			}
		}

		_, _, _ = syscall.RawSyscall(syscall.SYS_SCHED_YIELD, 0, 0, 0)
		time.Sleep(time.Millisecond * 200)
	}
}

func killContainerCb(pod *pod, data []byte) error {
	var payload hyper.KillContainer

	if pod.running == false {
		return fmt.Errorf("Pod not started, impossible to signal the container")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	ctr := pod.getContainer(payload.ID)

	if ctr == nil {
		return fmt.Errorf("Container %s not found, impossible to signal", payload.ID)
	}

	// container creation (newcontainer) does not guarantee the execution of the workload
	// we have to wait a bit before send any signal
	// https://github.com/clearcontainers/runtime/issues/430
	waitForContainerWorkload(pod.containers[payload.ID].container)

	// Use AllProcesses to make sure we carry forward the flag passed by the runtime.
	if err := ctr.container.Signal(payload.Signal, payload.AllProcesses); err != nil {
		return err
	}

	return nil
}

func (c *container) removeContainer(id string) error {
	c.wgProcesses.Wait()

	if err := c.container.Destroy(); err != nil {
		return err
	}

	return unmountContainerRootFs(id, c.config.Rootfs)
}

func removeContainerCb(pod *pod, data []byte) error {
	var payload hyper.RemoveContainer

	if pod.running == false {
		return fmt.Errorf("Pod not started, impossible to remove the container")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	ctr := pod.getContainer(payload.ID)

	if ctr == nil {
		return fmt.Errorf("Container %s not found, impossible to remove", payload.ID)
	}

	status, err := ctr.container.Status()
	if err != nil {
		return err
	}

	if status == libcontainer.Running {
		return fmt.Errorf("Container %s running, impossible to remove", payload.ID)
	}

	if err := ctr.removeContainer(payload.ID); err != nil {
		return err
	}

	pod.deleteContainer(payload.ID)

	return nil
}

func execCb(pod *pod, data []byte) error {
	var payload hyper.Exec

	if pod.running == false {
		return fmt.Errorf("Pod not started, impossible to exec process on the container")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	if payload.Process.ID == "" {
		payload.Process.ID = fmt.Sprintf("%d", payload.Process.Stdio)
	}

	ctr := pod.getContainer(payload.ContainerID)

	if ctr == nil {
		return fmt.Errorf("Container %s not found, impossible to execute process %s", payload.ContainerID, payload.Process.ID)
	}

	status, err := ctr.container.Status()
	if err != nil {
		return err
	}

	if status != libcontainer.Running {
		return fmt.Errorf("Container %s not running, impossible to execute process %s", payload.ContainerID, payload.Process.ID)
	}

	process := ctr.getProcess(payload.Process.ID)

	if process != nil {
		return fmt.Errorf("Process %s already exists", payload.Process.ID)
	}

	process, err = pod.buildProcess(payload.Process)
	if err != nil {
		return err
	}

	ctr.setProcess(payload.Process.ID, process)

	ctr.wgProcesses.Add(1)

	started := make(chan error)
	go pod.runContainerProcess(payload.ContainerID, payload.Process.ID, payload.Process.Terminal, started)

	select {
	case err := <-started:
		if err != nil {
			return fmt.Errorf("Process could not be started: %v", err)
		}
	case <-time.After(time.Duration(5) * time.Second):
		return fmt.Errorf("Process could not be started: timeout error")
	}

	return nil
}

func readyCb(pod *pod, data []byte) error {
	return nil
}

func pingCb(pod *pod, data []byte) error {
	return nil
}

func (p *pod) findTermFromSeqID(seqID uint64) (*os.File, string, error) {
	p.containersLock.RLock()
	defer p.containersLock.RUnlock()

	for cid, container := range p.containers {
		container.processesLock.RLock()

		for _, process := range container.processes {
			if process.seqStdio == seqID {
				container.processesLock.RUnlock()
				return process.termMaster, cid, nil
			}
		}
		container.processesLock.RUnlock()
	}

	return nil, "", fmt.Errorf("Could not find a process related to sequence %d", seqID)
}

func winsizeCb(pod *pod, data []byte) error {
	var payload hyper.Winsize

	if pod.running == false {
		return fmt.Errorf("Pod not started, impossible to resize the window")
	}

	if err := json.Unmarshal(data, &payload); err != nil {
		return err
	}

	term, cid, err := pod.findTermFromSeqID(payload.Sequence)
	if err != nil {
		return err
	}

	ctr := pod.getContainer(cid)
	status, err := ctr.container.Status()

	if err != nil {
		return err
	}

	if status != libcontainer.Running {
		return fmt.Errorf("Container %s not running, impossible to resize window", cid)
	}

	window := new(struct {
		Row    uint16
		Col    uint16
		Xpixel uint16
		Ypixel uint16
	})

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		term.Fd(),
		syscall.TIOCGWINSZ,
		uintptr(unsafe.Pointer(window))); errno != 0 {
		return fmt.Errorf("Could not get window size: %v", errno.Error())
	}

	window.Row = payload.Row
	window.Col = payload.Column

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL,
		term.Fd(),
		syscall.TIOCSWINSZ,
		uintptr(unsafe.Pointer(window))); errno != 0 {
		return fmt.Errorf("Could not set window size: %v", errno.Error())
	}

	return nil
}

func newConfig(level logrus.Level) agentConfig {
	config := agentConfig{}
	config.logLevel = level
	return config
}

//Get the agent configuration from kernel cmdline
func (c *agentConfig) getConfig(cmdLineFile string) error {

	if cmdLineFile == "" {
		return fmt.Errorf("Kernel cmdline file cannot be empty")
	}

	kernelCmdline, err := ioutil.ReadFile(cmdLineFile)
	if err != nil {
		return err
	}

	words := strings.Fields(string(kernelCmdline))
	for _, w := range words {
		word := string(w)
		if err := c.parseCmdlineOption(word); err != nil {
			agentLog.Warnf("Failed to parse kernel option %s: %v", word, err)
		}
	}
	return nil
}

func applyConfig(config agentConfig) {
	agentLog.SetLevel(config.logLevel)
}

//Parse a string that represents a kernel cmdline option
func (c *agentConfig) parseCmdlineOption(option string) error {

	const (
		optionPosition = iota
		valuePosition
		optionSeparator = "="
	)

	split := strings.Split(option, optionSeparator)

	if len(split) < valuePosition+1 {
		return nil
	}

	switch split[optionPosition] {
	case logLevelFlag:
		level, err := logrus.ParseLevel(split[valuePosition])
		if err != nil {
			return err
		}
		c.logLevel = level
	default:
		if strings.HasPrefix(split[optionPosition], optionPrefix) {
			return fmt.Errorf("Unknown option %s", split[optionPosition])
		}
	}
	return nil

}
