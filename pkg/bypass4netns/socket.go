package bypass4netns

import (
	"fmt"
	"syscall"
	"unsafe"

	"github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

type socketOption struct {
	level   uint64
	optname uint64
	optval  []byte
	optlen  uint64
}

// Handle F_SETFL, F_SETFD options
type fcntlOption struct {
	cmd   uint64
	value uint64
}

type socketState int

const (
	// NotBypassableSocket  means that the fd is not socket or not bypassed
	NotBypassable socketState = iota

	// NotBypassed means that the socket is not bypassed.
	NotBypassed

	// Bypassed means that the socket is replaced by one created on the host
	Bypassed

	// SwitchBacked means that the socket was bypassed but now rereplaced to the socket in netns.
	// This state can be hannpend in connect(2), sendto(2) and sendmsg(2)
	// when connecting to a host outside of netns and then connecting to a host inside of netns with same fd.
	//SwitchBacked
)

func (ss socketState) String() string {
	switch ss {
	case NotBypassable:
		return "NotBypassable"
	case NotBypassed:
		return "NotBypassed"
	case Bypassed:
		return "Bypassed"
	//case SwitchBacked:
	//	return "SwitchBacked"
	default:
		panic(fmt.Sprintf("unexpected enum %d: String() is not implmented", ss))
	}
}

type processStatus struct {
	sockets map[int]*socketStatus
}

func newProcessStatus() *processStatus {
	return &processStatus{
		sockets: map[int]*socketStatus{},
	}
}

type socketStatus struct {
	state      socketState
	pid        uint32
	sockfd     int
	sockDomain int
	sockType   int
	sockProto  int
	// address for bind or connect
	addr          *sockaddr
	socketOptions []socketOption
	fcntlOptions  []fcntlOption

	logger *logrus.Entry
}

func newSocketStatus(pid uint32, sockfd int, sockDomain, sockType, sockProto int) *socketStatus {
	return &socketStatus{
		state:         NotBypassed,
		pid:           pid,
		sockfd:        sockfd,
		sockDomain:    sockDomain,
		sockType:      sockType,
		sockProto:     sockProto,
		socketOptions: []socketOption{},
		fcntlOptions:  []fcntlOption{},
		logger:        logrus.WithFields(logrus.Fields{"pid": pid, "sockfd": sockfd}),
	}
}

func (ss *socketStatus) handleSysSetsockopt(ctx *context) error {
	ss.logger.Debug("handle setsockopt")
	level := ctx.req.Data.Args[1]
	optname := ctx.req.Data.Args[2]
	optlen := ctx.req.Data.Args[4]
	optval, err := readProcMem(ctx.req.Pid, ctx.req.Data.Args[3], optlen)
	if err != nil {
		return fmt.Errorf("readProcMem failed pid %v offset 0x%x: %s", ctx.req.Pid, ctx.req.Data.Args[1], err)
	}

	value := socketOption{
		level:   level,
		optname: optname,
		optval:  optval,
		optlen:  optlen,
	}
	ss.socketOptions = append(ss.socketOptions, value)

	ss.logger.Infof("setsockopt level=%d optname=%d optval=%v optlen=%d was recorded.", level, optname, optval, optlen)
	return nil
}

func (ss *socketStatus) handleSysFcntl(ctx *context) {
	ss.logger.Debug("handle fcntl")
	fcntlCmd := ctx.req.Data.Args[1]
	switch fcntlCmd {
	case unix.F_SETFD: // 0x2
	case unix.F_SETFL: // 0x4
		opt := fcntlOption{
			cmd:   fcntlCmd,
			value: ctx.req.Data.Args[2],
		}
		ss.fcntlOptions = append(ss.fcntlOptions, opt)
		ss.logger.Infof("fcntl cmd=0x%x value=%d was recorded.", fcntlCmd, opt.value)
	case unix.F_GETFL: // 0x3
		// ignore these
	default:
		ss.logger.Warnf("Unknown fcntl command 0x%x ignored.", fcntlCmd)
	}
}

func (ss *socketStatus) handleSysConnect(handler *notifHandler, ctx *context) {
	destAddr, err := readSockaddrFromProcess(ss.pid, ctx.req.Data.Args[1], ctx.req.Data.Args[2])
	if err != nil {
		ss.logger.Errorf("failed to read sockaddr from process: %q", err)
		return
	}
	ss.addr = destAddr

	isNotBypassed := handler.nonBypassable.Contains(destAddr.IP)
	if isNotBypassed {
		ss.logger.Infof("destination address %v is not bypassed.", destAddr.IP)
		ss.state = NotBypassable
		return
	}

	sockfdOnHost, err := syscall.Socket(ss.sockDomain, ss.sockType, ss.sockProto)
	if err != nil {
		ss.logger.Errorf("failed to create socket: %q", err)
		ss.state = NotBypassable
		return
	}
	defer syscall.Close(sockfdOnHost)

	err = ss.configureSocket(sockfdOnHost)
	if err != nil {
		ss.logger.Errorf("failed to configure socket: %q", err)
		ss.state = NotBypassable
		return
	}

	addfd := seccompNotifAddFd{
		id:         ctx.req.ID,
		flags:      SeccompAddFdFlagSetFd,
		srcfd:      uint32(sockfdOnHost),
		newfd:      uint32(ctx.req.Data.Args[0]),
		newfdFlags: 0,
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		ss.logger.Errorf("ioctl NotifAddFd failed: %q", err)
		ss.state = NotBypassable
		return
	}

	ss.state = Bypassed
	ss.logger.Infof("bypassed connect socket destAddr=%s", ss.addr)
}

func (ss *socketStatus) handleSysBind(handler *notifHandler, ctx *context) {
	sa, err := readSockaddrFromProcess(ctx.req.Pid, ctx.req.Data.Args[1], ctx.req.Data.Args[2])
	if err != nil {
		ss.logger.Errorf("failed to read sockaddr from process: %q", err)
		ss.state = NotBypassable
		return
	}
	ss.addr = sa

	ss.logger.Infof("handle port=%d, ip=%v", sa.Port, sa.IP)

	// TODO: get port-fowrad mapping from nerdctl
	fwdPort, ok := handler.forwardingPorts[int(sa.Port)]
	if !ok {
		ss.logger.Infof("port=%d is not target of port forwarding.", sa.Port)
		ss.state = NotBypassable
		return
	}

	sockfdOnHost, err := syscall.Socket(ss.sockDomain, ss.sockType, ss.sockProto)
	if err != nil {
		ss.logger.Errorf("failed to create socket: %q", err)
		ss.state = NotBypassable
		return
	}
	defer syscall.Close(sockfdOnHost)

	err = ss.configureSocket(sockfdOnHost)
	if err != nil {
		ss.logger.Errorf("failed to configure socket: %q", err)
		ss.state = NotBypassable
		return
	}

	var bind_addr syscall.Sockaddr

	switch sa.Family {
	case syscall.AF_INET:
		var addr [4]byte
		for i := 0; i < 4; i++ {
			addr[i] = sa.IP[i]
		}
		bind_addr = &syscall.SockaddrInet4{
			Port: fwdPort.HostPort,
			Addr: addr,
		}
	case syscall.AF_INET6:
		var addr [16]byte
		for i := 0; i < 16; i++ {
			addr[i] = sa.IP[i]
		}
		bind_addr = &syscall.SockaddrInet6{
			Port:   fwdPort.HostPort,
			ZoneId: sa.ScopeID,
			Addr:   addr,
		}
	}

	err = syscall.Bind(sockfdOnHost, bind_addr)
	if err != nil {
		ss.logger.Errorf("bind failed: %s", err)
		ss.state = NotBypassable
		return
	}

	addfd := seccompNotifAddFd{
		id:         ctx.req.ID,
		flags:      SeccompAddFdFlagSetFd,
		srcfd:      uint32(sockfdOnHost),
		newfd:      uint32(ctx.req.Data.Args[0]),
		newfdFlags: 0,
	}

	err = addfd.ioctlNotifAddFd(ctx.notifFd)
	if err != nil {
		ss.logger.Errorf("ioctl NotifAddFd failed: %s", err)
		ss.state = NotBypassable
		return
	}

	ss.state = Bypassed
	ss.logger.Infof("bypassed bind socket for %d:%d is done", fwdPort.HostPort, fwdPort.ChildPort)

	ctx.resp.Flags &= (^uint32(SeccompUserNotifFlagContinue))
}

func (ss *socketStatus) configureSocket(sockfd int) error {
	for _, optVal := range ss.socketOptions {
		_, _, errno := syscall.Syscall6(syscall.SYS_SETSOCKOPT, uintptr(sockfd), uintptr(optVal.level), uintptr(optVal.optname), uintptr(unsafe.Pointer(&optVal.optval[0])), uintptr(optVal.optlen), 0)
		if errno != 0 {
			return fmt.Errorf("setsockopt failed(%v): %s", optVal, errno)
		}
		ss.logger.Debugf("configured socket option val=%v", optVal)
	}

	for _, fcntlVal := range ss.fcntlOptions {
		_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(sockfd), uintptr(fcntlVal.cmd), uintptr(fcntlVal.value))
		if errno != 0 {
			return fmt.Errorf("fnctl failed(%v): %s", fcntlVal, errno)
		}
		ss.logger.Debugf("configured socket fcntl val=%v", fcntlVal)
	}

	return nil
}
