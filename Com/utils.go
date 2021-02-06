package Com

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"reflect"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	SoOriginalDst    = 80
	Ip6SoOriginalDst = 80 // from linux/include/uapi/linux/netfilter_ipv6/ip6_tables.h
)

// get origin destination addr
func GetTcpRemoteAddr(conn *net.TCPConn) (*net.TCPAddr, error) {
	// get file descriptor
	file, err := conn.File()
	if err != nil {
		return nil, err
	}
	fd := int(file.Fd())

	// from linux/include/uapi/linux/netfilter_ipv4.h
	req, err := unix.GetsockoptIPv6Mreq(fd, syscall.IPPROTO_IP, SoOriginalDst)
	if err != nil {
		return nil, err
	}

	// struct tcp addr
	tcpAddr := &net.TCPAddr{
		IP:   req.Multiaddr[4:8],
		Port: int(req.Multiaddr[2])<<8 + int(req.Multiaddr[3]),
	}
	return tcpAddr, nil
}

// set conn opt transparent
func SetConnOptTrn(conn net.Conn) error {
	// check if is the same type, udp addr can not dial tcp addr
	if reflect.TypeOf(conn) != reflect.TypeOf(net.UDPConn{}) && reflect.TypeOf(conn) != reflect.TypeOf(net.TCPConn{}) {
		return errors.New("conn type is not udp conn and tcp conn")
	}
	/*
		udp conn and tcp conn have all File() method
			type conn struct {
				fd *netFD
			}
			func (c *conn) File() (f *os.File, err error)
	*/
	// call File() method
	value := reflect.ValueOf(conn)
	call := value.MethodByName("File").Call(nil)
	if len(call) != 2 {
		return errors.New("return of file method is not match")
	}
	// check err
	if err, ok := call[1].Interface().(error); !ok {
		return errors.New("convert error failed")
	} else if err != nil {
		return err
	}
	// convert file
	file, ok := call[0].Interface().(*os.File)
	if !ok {
		return errors.New("convert file failed")
	}
	// set sock opt trn
	return SetSockOptTrn(int(file.Fd()))
}

// set socket transparent
func SetSockOptTrn(fd int) error {
	soTyp, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_TYPE)
	if err != nil {
		return err
	}
	// check if type match
	if soTyp != syscall.SOCK_STREAM && soTyp != syscall.SOCK_DGRAM {
		return errors.New("sock type is not tcp and udp")
	}
	// set reuse addr
	if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 1); err != nil {
		return err
	}
	// set ip transparent
	if err = syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.IP_TRANSPARENT, 1); err != nil {
		return err
	}
	return nil
}

// addr type for udp and tcp
type BaseAddr struct {
	IP   net.IP
	Port int
}

// parse origin remote addr msg from msg_hdr
func ParseRemoteAddrFromMsgHdr(buf []byte) (*BaseAddr, error) {
	var addr *BaseAddr
	if buf == nil {
		return addr, errors.New("parse buf is nil")
	}
	// parse control message
	msgSl, err := syscall.ParseSocketControlMessage(buf)
	if err != nil {
		return addr, err
	}
	// tcp and udp addr is the same struct, use tcp to represent all
	for _, msg := range msgSl {
		// use t_proxy and ip route, msg_hdr address is marked as sol_ip type
		if msg.Header.Level == syscall.SOL_IP && msg.Header.Type == syscall.IP_RECVORIGDSTADDR {
			addr = &BaseAddr{
				IP:   msg.Data[4:8],
				Port: int(binary.BigEndian.Uint16(msg.Data[2:4])),
			}
		} else if msg.Header.Level == syscall.SOL_IPV6 && msg.Header.Type == syscall.IP_RECVORIGDSTADDR {
			addr = &BaseAddr{
				IP:   msg.Data[8:24],
				Port: int(binary.BigEndian.Uint16(msg.Data[2:4])),
			}
		}
	}
	// check if addr is nil
	if addr == nil {
		err = errors.New("sol_ip type is not found int msg_hdr")
	}
	return addr, err
}

// mega dial try to transparent connect, privilege should be needed
func MegaDial(network string, lAddr net.Addr, rAddr net.Addr) (net.Conn, error) {
	// check if is the same type, udp addr can not dial tcp addr
	if reflect.TypeOf(lAddr) != reflect.TypeOf(rAddr) {
		return nil, errors.New("dial local addr is not match with remote addr")
	}
	// get domain
	var domain int
	var ip net.IP = reflect.ValueOf(lAddr).FieldByName("IP").Bytes()
	if ip.To4() != nil {
		domain = syscall.AF_INET
	} else if ip.To16() != nil {
		domain = syscall.AF_INET6
	} else {
		return nil, errors.New("local ip is incorrect")
	}
	// get typ
	var typ int
	if network == "tcp" {
		typ = syscall.SOCK_STREAM
	} else if network == "udp" {
		typ = syscall.SOCK_DGRAM
	}
	fd, err := syscall.Socket(domain, typ, 0)
	if err != nil {
		return nil, err
	}
	// set transparent
	if err = SetSockOptTrn(fd); err != nil {
		return nil, err
	}
	// convert addr
	lSockAddr, err := convertAddrToSockAddr(lAddr)
	if err != nil {
		return nil, err
	}
	rSockAddr, err := convertAddrToSockAddr(rAddr)
	if err != nil {
		return nil, err
	}
	// bind fake addr
	if err = syscall.Bind(fd, lSockAddr); err != nil {
		return nil, err
	}
	// bind addr
	if err = syscall.Connect(fd, rSockAddr); err != nil {
		return nil, err
	}
	// create new file
	file := os.NewFile(uintptr(fd), fmt.Sprintf("udp_handler_%v", fd))
	if file == nil {
		return nil, errors.New("create new file is nil")
	}
	// create file conn
	conn, err := net.FileConn(file)
	if err != nil {
		return nil, err
	}
	// debug message
	return conn, nil
}

// convert addr to sock addr
func convertAddrToSockAddr(addr net.Addr) (syscall.Sockaddr, error) {
	// check if addr can convert to udp addr and tcp addr, if not return as error
	if !reflect.TypeOf(addr).ConvertibleTo(reflect.TypeOf(net.UDPAddr{})) &&
		!reflect.TypeOf(addr).ConvertibleTo(reflect.TypeOf(net.TCPAddr{})) {
		return nil, errors.New("addr typ is not tcp addr or udp addr")
	}
	// convert net addr to sock_addr
	value := reflect.ValueOf(addr)
	var ip net.IP = value.FieldByName("IP").Bytes()
	port := value.FieldByName("Port").Int()
	if port == 0 {
		port = 80
	}
	// convert addr and port
	if ip.To4() != nil {
		inet4 := &syscall.SockaddrInet4{
			Port: int(port),
		}
		copy(inet4.Addr[:], ip.To4())
		return inet4, nil
	} else if ip.To16() != nil {
		inet6 := &syscall.SockaddrInet6{
			Port: int(port),
		}
		copy(inet6.Addr[:], ip.To16())
		return inet6, nil
	}
	return nil, errors.New("ip is not ipv4 or ipv6")
}

type DataPackage struct {
	Addr net.Addr
	Data []byte
}

// marshal data, now only useful for udp
func MarshalPackage(pkg DataPackage, proto string) []byte {
	/*
			sock5 udp data
		   +----+------+--------+----------+----------+------+
		   |RSV | FRAG |  ATYP  | DST.ADDR | DST.PORT | DATA |
		   +----+------+--------+------+----------+----------+
		   | 1  |  0   |    1   | Variable | Variable | Data |
		   +----+------+--------+----------+----------+------+
	*/
	// message
	addr := pkg.Addr
	var ip net.IP = reflect.ValueOf(addr).FieldByName("IP").Bytes()
	port16 := reflect.ValueOf(addr).FieldByName("Port").Uint()
	data := pkg.Data
	// udp message protocol
	buf := make([]byte, 4)
	buf[0] = 0
	// only udp is valid
	switch proto {
	case "tcp":
		return nil
	case "udp":
		buf[1] = 0
	default:
		return nil
	}
	buf[1] = 0
	buf[2] = 0
	if ip.To4() != nil {
		buf[3] = 1
		buf = append(buf, ip.To4()...)
	} else if ip.To16() != nil {
		buf[3] = 1
		buf = append(buf, ip.To16()...)
	} else {
		buf[3] = 3
		buf = append(buf, ip...)
	}
	// convert port 2 byte

	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, uint16(port16))
	buf = append(buf, port...)
	// add data
	buf = append(buf, data...)
	return buf
}

func UnMarshalPackage(msg []byte) DataPackage {
	addr := msg[4:8]
	port := binary.BigEndian.Uint16(msg[8:10])
	data := msg[10:]

	return DataPackage{
		Addr: &net.UDPAddr{
			IP:   addr[:],
			Port: int(port),
		},
		Data: data,
	}
}
