package remote

import (
	"context"
	"encoding/binary"
	"fmt"
	"lproxy_tun/meta"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const (
	readBufSize  = 16 * 1024
	writeBufSize = 16 * 1024
)

const (
	cMDNone              = 0
	cMDReqBegin          = 1
	cMDReqData           = 1
	cMDReqCreated        = 2
	cMDReqClientClosed   = 3
	cMDReqClientFinished = 4
	cMDReqServerFinished = 5
	cMDReqServerClosed   = 6
	cMDReqEnd            = 7
	cMDDNSReq            = 7
	cMDDNSRsp            = 8
	cMDUDPReq            = 9
)

type WSTunnel struct {
	websocketURL string
	reqq         *Reqq

	isActivated bool

	protector func(fd uint64)

	wsLock   sync.Mutex
	ws       *websocket.Conn
	waitping int

	cache *Cache
	mgr   *Mgr
}

func newTunnel(websocketURL string, reqCap int) *WSTunnel {
	wst := &WSTunnel{
		websocketURL: websocketURL,
		cache:        newCache(),
	}

	reqq := newReqq(reqCap, wst)
	wst.reqq = reqq

	return wst
}

func (tnl *WSTunnel) start() {
	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	if tnl.isActivated {
		return
	}

	tnl.isActivated = true

	go tnl.serveWebsocket()
}

func (tnl *WSTunnel) stop() {
	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	tnl.isActivated = false

	if tnl.ws != nil {
		tnl.ws.Close()
		tnl.ws = nil
	}

	tnl.reqq.cleanup()
	tnl.cache.cleanup()
}

func (tnl *WSTunnel) serveWebsocket() {
	delayfn := func(eCount int) {
		tick := 3 * eCount
		if tick > 15 {
			tick = 15
		} else if tick < 3 {
			tick = 3
		}

		time.Sleep(time.Duration(tick) * time.Second)
	}

	failedConnect := 0
	for tnl.isActivated {
		// connect
		conn, err := tnl.dail()
		if err != nil {
			log.Errorf("dial %s, %s", tnl.websocketURL, err.Error())
			failedConnect++
			delayfn(failedConnect)
			continue
		}

		tnl.onConnected(conn)

		failedConnect = 0

		// read
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Errorf("websocket ReadMessage error: %v", err)
				}
				break
			}

			tnl.processWebsocketMsg(message)
		}

		tnl.onDisconnected()
	}
}

func (tnl *WSTunnel) onConnected(conn *websocket.Conn) {
	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	if !tnl.isActivated {
		conn.Close()
		tnl.ws = nil

		return
	}

	conn.SetPingHandler(func(data string) error {
		tnl.sendPong([]byte(data))
		return nil
	})

	conn.SetPongHandler(func(data string) error {
		tnl.onPong([]byte(data))
		return nil
	})

	// save for sending
	tnl.ws = conn
}

func (tnl *WSTunnel) onDisconnected() {
	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	tnl.ws.Close()
	tnl.ws = nil
}

func (tnl *WSTunnel) keepalive() {
	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	if !tnl.isValid() {
		return
	}

	conn := tnl.ws
	if conn == nil {
		return
	}

	now := time.Now().Unix()
	data := make([]byte, 8)
	binary.LittleEndian.PutUint64(data, uint64(now))

	if tnl.waitping > 3 {
		conn.Close()
		return
	}

	err := conn.WriteMessage(websocket.PingMessage, data)
	if err != nil {
		log.Errorf("websocket send PingMessage error:%v", err)
	}

	tnl.waitping++
}

func (tnl *WSTunnel) sendPong(data []byte) {
	if !tnl.isActivated {
		return
	}

	conn := tnl.ws
	if conn == nil {
		return
	}

	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	err := conn.WriteMessage(websocket.PongMessage, data)
	if err != nil {
		log.Errorf("websocket send PongMessage error:%v", err)
	}
}

func (tnl *WSTunnel) onPong(_ []byte) {
	tnl.waitping = 0
}

func (tnl *WSTunnel) send(data []byte) {
	if !tnl.isActivated {
		return
	}

	conn := tnl.ws
	if conn == nil {
		return
	}

	tnl.wsLock.Lock()
	defer tnl.wsLock.Unlock()

	err := conn.WriteMessage(websocket.BinaryMessage, data)
	if err != nil {
		log.Errorf("websocket WriteMessage error:%v", err)
	}
}

func (tnl *WSTunnel) dail() (*websocket.Conn, error) {
	d := websocket.Dialer{
		ReadBufferSize:   readBufSize,
		WriteBufferSize:  writeBufSize,
		HandshakeTimeout: 5 * time.Second,
		NetDialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			var d *net.Dialer
			if tnl.protector != nil {
				d = &net.Dialer{
					Control: func(network, address string, c syscall.RawConn) error {
						c.Control(func(fd uintptr) {
							tnl.protector(uint64(fd))
						})
						return nil
					},
				}
			} else {
				d = &net.Dialer{}
			}

			return d.DialContext(ctx, network, addr)
		},
	}

	conn, _, err := d.Dial(tnl.websocketURL, nil)
	if err != nil {
		return nil, err
	}

	return conn, nil
}

func (tnl *WSTunnel) processWebsocketMsg(msg []byte) {
	if len(msg) < 1 {
		log.Error("WSTunnel.processWebsocketMsg empty msg")
		return
	}

	cmd := msg[0]
	if cmd >= cMDReqBegin && cmd < cMDReqEnd {
		tnl.processReqMsg(msg)
	} else if cmd == cMDUDPReq {
		tnl.onServerUDPData(msg)
	}
}

func (tnl *WSTunnel) processReqMsg(msg []byte) {
	cmd := msg[0]
	idx := binary.LittleEndian.Uint16(msg[1:])
	tag := binary.LittleEndian.Uint16(msg[3:])

	switch cmd {
	case cMDReqData:
		tnl.onServerReqData(idx, tag, msg[5:])
	case cMDReqServerFinished:
		tnl.onSeverReqHalfClosed(idx, tag)
	case cMDReqServerClosed:
		tnl.onServerReqClosed(idx, tag)
	case cMDReqCreated:
		tnl.onServerReqCreate(idx, tag, msg[5:])

	}
}

func (tnl *WSTunnel) onServerReqData(idx, tag uint16, msg []byte) {
	req, err := tnl.reqq.get(idx, tag)
	if err != nil {
		log.Errorf("WSTunnel.onServerReqData error:%v", err)
		return
	}

	err = req.onServerData(msg)
	if err != nil {
		log.Errorf("WSTunnel.onServerReqData call req.onServerData error:%v", err)
	}
}

func (tnl *WSTunnel) onSeverReqHalfClosed(idx, tag uint16) {
	req, err := tnl.reqq.get(idx, tag)
	if err != nil {
		log.Errorf("WSTunnel.onServerReqData error:%v", err)
		return
	}

	req.onSeverHalfClosed()
}

func (tnl *WSTunnel) onServerReqClosed(idx, tag uint16) {
	tnl.freeReq(idx, tag)
}

func (tnl *WSTunnel) onServerReqCreate(idx, tag uint16, message []byte) {
	addressType := message[0]
	var port uint16
	var domain string
	switch addressType {
	case 0: // ipv4
		domain = fmt.Sprintf("%d.%d.%d.%d", message[1], message[2], message[3], message[4])
		port = binary.LittleEndian.Uint16(message[5:])
	case 1: // domain name
		domainLen := message[1]
		domain = string(message[2 : 2+domainLen])
		port = binary.LittleEndian.Uint16(message[(2 + domainLen):])
	case 2: // ipv6
		p1 := binary.LittleEndian.Uint16(message[1:])
		p2 := binary.LittleEndian.Uint16(message[3:])
		p3 := binary.LittleEndian.Uint16(message[5:])
		p4 := binary.LittleEndian.Uint16(message[7:])
		p5 := binary.LittleEndian.Uint16(message[9:])
		p6 := binary.LittleEndian.Uint16(message[11:])
		p7 := binary.LittleEndian.Uint16(message[13:])
		p8 := binary.LittleEndian.Uint16(message[15:])

		domain = fmt.Sprintf("%d:%d:%d:%d:%d:%d:%d:%d", p8, p7, p6, p5, p4, p3, p2, p1)
		port = binary.LittleEndian.Uint16(message[17:])
	default:
		log.Info("onServerReqCreate, not support addressType:", addressType)
		return
	}

	req, err := tnl.reqq.allocForReverseProxy(idx, tag)
	if err != nil {
		log.Info("onServerReqCreate, alloc req failed:", err)
		return
	}

	addr := fmt.Sprintf("%s:%d", domain, port)
	log.Info("proxy to ", addr)

	srcAddr, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		log.Errorf("onServerReqCreate resolveTCPAddr %s failed %s", addr, err.Error())
		return
	}

	conn, err := tnl.newTCP(srcAddr)
	if err != nil {
		log.Errorf("onServerReqCreate newTCP %s failed %s", addr, err.Error())
		tnl.onClientTerminate(req.idx, req.tag)
		return
	}

	req.conn = conn
	go req.proxy()
}

func (tnl *WSTunnel) isValid() bool {
	return tnl.isActivated && tnl.ws != nil
}

func (tnl *WSTunnel) onClientTerminate(idx uint16, tag uint16) {
	buf := make([]byte, 5)
	buf[0] = cMDReqClientClosed
	binary.LittleEndian.PutUint16(buf[1:], idx)
	binary.LittleEndian.PutUint16(buf[3:], tag)

	tnl.send(buf)

	tnl.freeReq(idx, tag)
}

func (tnl *WSTunnel) freeReq(idx, tag uint16) {
	err := tnl.reqq.free(idx, tag)
	if err != nil {
		log.Errorf("WSTunnel.freeReq, get req failed:%v", err)
		return
	}
}

func (tnl *WSTunnel) onClientHalfClosed(idx uint16, tag uint16) {
	buf := make([]byte, 5)
	buf[0] = cMDReqClientFinished
	binary.LittleEndian.PutUint16(buf[1:], idx)
	binary.LittleEndian.PutUint16(buf[3:], tag)

	tnl.send(buf)
}

func (tnl *WSTunnel) onClientReqData(idx uint16, tag uint16, data []byte) {
	buf := make([]byte, 5+len(data))
	buf[0] = cMDReqData
	binary.LittleEndian.PutUint16(buf[1:], idx)
	binary.LittleEndian.PutUint16(buf[3:], tag)
	copy(buf[5:], data)

	tnl.send(buf)
}

func (tnl *WSTunnel) onClientCreate(conn meta.TCPConn, idx, tag uint16) {
	addr := conn.ID().LocalAddress
	port := conn.ID().LocalPort
	log.Infof("local address %s local port %d", addr, port)

	iplen := addr.Len()

	buf := make([]byte, 8+iplen)
	buf[0] = cMDReqCreated
	binary.LittleEndian.PutUint16(buf[1:], idx)
	binary.LittleEndian.PutUint16(buf[3:], tag)

	if iplen > 4 {
		// ipv6
		buf[5] = 2
		src := addr.As16()
		copy(buf[6:], src[:])
	} else {
		buf[5] = 0
		src := addr.As4()
		copy(buf[6:], src[:])
	}

	binary.LittleEndian.PutUint16(buf[6+iplen:], uint16(port))

	tnl.send(buf)
}

func (tnl *WSTunnel) acceptTCPConn(conn meta.TCPConn) error {
	req, err := tnl.reqq.alloc(conn)
	if err != nil {
		return err
	}

	tnl.onClientCreate(conn, req.idx, req.tag)

	// start a new goroutine to read data from 'conn'
	go req.proxy()

	return nil
}

func (tnl *WSTunnel) acceptUDPConn(conn meta.UDPConn) error {
	src := &net.UDPAddr{Port: int(conn.ID().RemotePort), IP: conn.ID().RemoteAddress.AsSlice()}
	dest := &net.UDPAddr{Port: int(conn.ID().LocalPort), IP: conn.ID().LocalAddress.AsSlice()}

	log.Infof("acceptUDPConn src %s dest %s", src.String(), dest.String())

	ustub := tnl.cache.get(src, dest)
	if ustub != nil {
		return fmt.Errorf("conn src %s dest %s already exist", src.String(), dest.String())
	}

	ustub = newUstub(tnl, conn)
	tnl.cache.add(ustub)
	go ustub.proxy()

	return nil
}

func (tnl *WSTunnel) onServerUDPData(msg []byte) error {
	src := parseAddress(msg[1:])
	dest := parseAddress(msg[1+3+len(src.IP):])

	log.Debugf("onServerUDPData src %s dest %s", src.String(), dest.String())

	ustub := tnl.cache.get(src, dest)
	if ustub == nil {
		conn, err := tnl.newUDP(src, dest)
		if err != nil {
			log.Errorf("onServerUDPData new UDPConn src %s dest %s failed, %s", src.String(), dest.String(), err.Error())
			return nil
		}

		ustub = newUstub(tnl, conn)
		tnl.cache.add(ustub)
		go ustub.proxy()

		log.Infof("onServerUDPData, new UDPConn src %s dest %s for reverse proxy", src.String(), dest.String())
	}

	// 7 = cmd + ipType1 + port1 + ipType2 + port2
	skip := 7 + len(src.IP) + len(dest.IP)
	return ustub.writeTo(msg[skip:], src)
}

func (tnl *WSTunnel) onClientUDPData(msg []byte, src, dest *net.UDPAddr) error {
	log.Debugf("onClientUDPData src %s, dest %s", src, dest)
	srcAddrBuf := writeAddress(src)
	destAddrBuf := writeAddress(dest)

	buf := make([]byte, 1+len(srcAddrBuf)+len(destAddrBuf)+len(msg))

	buf[0] = byte(cMDUDPReq)
	copy(buf[1:], srcAddrBuf)
	copy(buf[1+len(srcAddrBuf):], destAddrBuf)
	copy(buf[1+len(srcAddrBuf)+len(destAddrBuf):], msg)

	tnl.send(buf)
	return nil
}

func (tnl *WSTunnel) newUDP(src, dest *net.UDPAddr) (meta.UDPConn, error) {
	if tnl.mgr.localGvisor == nil {
		return nil, fmt.Errorf("localGvisor == nil")
	}

	id := &stack.TransportEndpointID{
		LocalPort:     uint16(dest.Port),
		LocalAddress:  tcpip.AddrFromSlice(dest.IP),
		RemotePort:    uint16(src.Port),
		RemoteAddress: tcpip.AddrFromSlice(src.IP),
	}

	newUDP4, err := tnl.mgr.localGvisor.NewUDP4(id)
	if err != nil {
		return nil, fmt.Errorf("NewUDP4 failed:%s", err)
	}

	return newUDP4, nil
}

func (tnl *WSTunnel) newTCP(src *net.TCPAddr) (meta.TCPConn, error) {
	if tnl.mgr.localGvisor == nil {
		return nil, fmt.Errorf("localGvisor == nil")
	}

	id := &stack.TransportEndpointID{
		RemotePort: uint16(src.Port),
	}

	if src.IP.To4() != nil {
		id.RemoteAddress = tcpip.AddrFromSlice(src.IP.To4())
	} else {
		id.RemoteAddress = tcpip.AddrFromSlice(src.IP.To16())
	}

	newTCP4, err := tnl.mgr.localGvisor.NewTCP4(id)
	if err != nil {
		return nil, fmt.Errorf("NewTCP4 failed:%s", err)
	}

	return newTCP4, nil
}

func writeAddress(addrss *net.UDPAddr) []byte {
	// 3 = iptype(1) + port(2)
	buf := make([]byte, 3+len(addrss.IP))
	// add port
	binary.LittleEndian.PutUint16(buf[0:], uint16(addrss.Port))
	// set ip type
	if len(addrss.IP) > net.IPv4len {
		// ipv6
		buf[2] = 2
	} else {
		// ipv4
		buf[2] = 0
	}

	copy(buf[3:], addrss.IP)
	return buf
}

func parseAddress(msg []byte) *net.UDPAddr {
	offset := 0
	port := binary.LittleEndian.Uint16(msg[offset:])
	offset += 2

	ipType := msg[offset]
	offset += 1

	switch ipType {
	case 0:
		// ipv4
		ip := make([]byte, 4)
		copy(ip, msg[offset:offset+4])
		return &net.UDPAddr{IP: ip, Port: int(port)}
	case 2:
		// ipv6
		ip := make([]byte, 16)
		copy(ip, msg[offset:offset+16])
		return &net.UDPAddr{IP: ip, Port: int(port)}
	}

	return nil
}

func setSocketMark(fd, mark int) error {
	if err := syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_MARK, mark); err != nil {
		return os.NewSyscallError("failed to set mark", err)
	}
	return nil
}
