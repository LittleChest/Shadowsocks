package service

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"time"

	"github.com/database64128/shadowsocks-go/conn"
	"github.com/database64128/shadowsocks-go/router"
	"github.com/database64128/shadowsocks-go/socks5"
	"github.com/database64128/shadowsocks-go/zerocopy"
	"go.uber.org/zap"
)

// session keeps track of a UDP session.
type session struct {
	clientAddrPort                netip.AddrPort
	clientOobCache                []byte
	natConn                       *net.UDPConn
	natConnMTU                    int
	natConnSendCh                 chan queuedPacket
	natConnPacker                 zerocopy.Packer
	natConnUnpacker               zerocopy.Unpacker
	natConnFixedTargetAddrPort    netip.AddrPort
	natConnUseFixedTargetAddrPort bool
	serverConnPacker              zerocopy.Packer
	serverConnUnpacker            zerocopy.Unpacker
	maxClientPacketSize           int
}

// UDPSessionRelay is a session-based UDP relay service.
//
// Incoming UDP packets are dispatched to NAT sessions based on the client session ID.
type UDPSessionRelay struct {
	serverName               string
	listenAddress            string
	listenerFwmark           int
	mtu                      int
	preferIPv6               bool
	server                   zerocopy.UDPServer
	serverConn               *net.UDPConn
	router                   *router.Router
	logger                   *zap.Logger
	packetBufPool            *sync.Pool
	mu                       sync.Mutex
	wg                       sync.WaitGroup
	table                    map[uint64]*session
	relayServerConnToNatConn func(csid uint64, entry *session)
	relayNatConnToServerConn func(csid uint64, entry *session)
}

func NewUDPSessionRelay(
	batchMode, serverName, listenAddress string,
	listenerFwmark, mtu int,
	preferIPv6 bool,
	server zerocopy.UDPServer,
	router *router.Router,
	logger *zap.Logger,
) *UDPSessionRelay {
	packetBufSize := mtu - IPv4HeaderLength - UDPHeaderLength
	packetBufPool := &sync.Pool{
		New: func() any {
			b := make([]byte, fixedFrontHeadroom+packetBufSize+fixedRearHeadroom)
			return &b
		},
	}
	s := UDPSessionRelay{
		serverName:     serverName,
		listenAddress:  listenAddress,
		listenerFwmark: listenerFwmark,
		mtu:            mtu,
		preferIPv6:     preferIPv6,
		server:         server,
		router:         router,
		logger:         logger,
		packetBufPool:  packetBufPool,
		table:          make(map[uint64]*session),
	}
	s.setRelayServerConnToNatConnFunc(batchMode)
	s.setRelayNatConnToServerConnFunc(batchMode)
	return &s
}

// String implements the Service String method.
func (s *UDPSessionRelay) String() string {
	return fmt.Sprintf("UDP session relay service for %s", s.serverName)
}

// Start implements the Service Start method.
func (s *UDPSessionRelay) Start() error {
	serverConn, err, serr := conn.ListenUDP("udp", s.listenAddress, true, s.listenerFwmark)
	if err != nil {
		return err
	}
	if serr != nil {
		s.logger.Warn("An error occurred while setting socket options on serverConn",
			zap.String("server", s.serverName),
			zap.String("listenAddress", s.listenAddress),
			zap.Int("listenerFwmark", s.listenerFwmark),
			zap.NamedError("serr", serr),
		)
	}
	s.serverConn = serverConn

	go func() {
		oobBuf := make([]byte, conn.UDPOOBBufferSize)

		for {
			packetBufp := s.packetBufPool.Get().(*[]byte)
			packetBuf := *packetBufp
			recvBuf := packetBuf[fixedFrontHeadroom : len(packetBuf)-fixedRearHeadroom]

			n, oobn, flags, clientAddrPort, err := s.serverConn.ReadMsgUDPAddrPort(recvBuf, oobBuf)
			if err != nil {
				if errors.Is(err, net.ErrClosed) {
					s.packetBufPool.Put(packetBufp)
					break
				}

				s.logger.Warn("Failed to read packet from serverConn",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Error(err),
				)

				s.packetBufPool.Put(packetBufp)
				continue
			}
			err = conn.ParseFlagsForError(flags)
			if err != nil {
				s.logger.Warn("Failed to read packet from serverConn",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", clientAddrPort),
					zap.Error(err),
				)

				s.packetBufPool.Put(packetBufp)
				continue
			}
			packet := recvBuf[:n]

			// Workaround for https://github.com/golang/go/issues/52264
			clientAddrPort = conn.Tov4Mappedv6(clientAddrPort)

			csid, err := s.server.SessionInfo(packet)
			if err != nil {
				s.logger.Warn("Failed to extract session info from packet",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", clientAddrPort),
					zap.Int("packetLength", n),
					zap.Error(err),
				)

				s.packetBufPool.Put(packetBufp)
				continue
			}

			var (
				targetAddr    socks5.Addr
				hasTargetAddr bool
				payloadStart  int
				payloadLength int
			)

			s.mu.Lock()

			entry := s.table[csid]
			if entry == nil {
				serverConnUnpacker, err := s.server.NewUnpacker(packet, csid)
				if err != nil {
					s.logger.Warn("Failed to create unpacker for client session",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Uint64("clientSessionID", csid),
						zap.Int("packetLength", n),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}

				targetAddr, hasTargetAddr, payloadStart, payloadLength, err = serverConnUnpacker.UnpackInPlace(packetBuf, fixedFrontHeadroom, n)
				if err != nil {
					s.logger.Warn("Failed to unpack packet",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Uint64("clientSessionID", csid),
						zap.Int("packetLength", n),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}
				if !hasTargetAddr { // Unlikely for server unpackers.
					targetAddr = socks5.AddrFromAddrPort(clientAddrPort)
				}

				c, err := s.router.GetUDPClient(s.serverName, clientAddrPort, targetAddr)
				if err != nil {
					s.logger.Warn("Failed to get UDP client for new NAT session",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Stringer("targetAddress", targetAddr),
						zap.Uint64("clientSessionID", csid),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}

				natConnFixedTargetAddrPort, natConnMTU, natConnFwmark, natConnUseFixedTargetAddrPort := c.AddrPort()
				natConnPacker, natConnUnpacker, err := c.NewSession()
				if err != nil {
					s.logger.Warn("Failed to create new UDP client session",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Stringer("targetAddress", targetAddr),
						zap.Uint64("clientSessionID", csid),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}

				serverConnPacker, err := s.server.NewPacker(csid)
				if err != nil {
					s.logger.Warn("Failed to create packer for client session",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Uint64("clientSessionID", csid),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}

				natConn, err, serr := conn.ListenUDP("udp", "", false, natConnFwmark)
				if err != nil {
					s.logger.Warn("Failed to create UDP socket for new NAT session",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Stringer("targetAddress", targetAddr),
						zap.Uint64("clientSessionID", csid),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}
				if serr != nil {
					s.logger.Warn("An error occurred while setting socket options on natConn",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Stringer("targetAddress", targetAddr),
						zap.Uint64("clientSessionID", csid),
						zap.Error(serr),
					)
				}

				err = natConn.SetReadDeadline(time.Now().Add(natTimeout))
				if err != nil {
					s.logger.Warn("Failed to set read deadline on natConn",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Stringer("targetAddress", targetAddr),
						zap.Uint64("clientSessionID", csid),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}

				entry = &session{
					clientAddrPort:                clientAddrPort,
					natConn:                       natConn,
					natConnMTU:                    natConnMTU,
					natConnSendCh:                 make(chan queuedPacket, sendChannelCapacity),
					natConnPacker:                 natConnPacker,
					natConnUnpacker:               natConnUnpacker,
					natConnFixedTargetAddrPort:    natConnFixedTargetAddrPort,
					natConnUseFixedTargetAddrPort: natConnUseFixedTargetAddrPort,
					serverConnPacker:              serverConnPacker,
					serverConnUnpacker:            serverConnUnpacker,
				}

				if addr := clientAddrPort.Addr(); addr.Is4() || addr.Is4In6() {
					entry.maxClientPacketSize = s.mtu - IPv4HeaderLength - UDPHeaderLength
				} else {
					entry.maxClientPacketSize = s.mtu - IPv6HeaderLength - UDPHeaderLength
				}

				s.table[csid] = entry

				s.wg.Add(2)

				go func() {
					s.relayNatConnToServerConn(csid, entry)

					s.mu.Lock()
					close(entry.natConnSendCh)
					delete(s.table, csid)
					s.mu.Unlock()

					s.wg.Done()
				}()

				go func() {
					s.relayServerConnToNatConn(csid, entry)
					entry.natConn.Close()
					s.wg.Done()
				}()

				s.logger.Info("New UDP session",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", clientAddrPort),
					zap.Stringer("targetAddress", targetAddr),
					zap.Uint64("clientSessionID", csid),
				)
			} else {
				targetAddr, hasTargetAddr, payloadStart, payloadLength, err = entry.serverConnUnpacker.UnpackInPlace(packetBuf, fixedFrontHeadroom, n)
				if err != nil {
					s.logger.Warn("Failed to unpack packet",
						zap.String("server", s.serverName),
						zap.String("listenAddress", s.listenAddress),
						zap.Stringer("clientAddress", clientAddrPort),
						zap.Uint64("clientSessionID", csid),
						zap.Int("packetLength", n),
						zap.Error(err),
					)

					s.packetBufPool.Put(packetBufp)
					s.mu.Unlock()
					continue
				}
				if !hasTargetAddr { // Unlikely for server unpackers.
					targetAddr = socks5.AddrFromAddrPort(clientAddrPort)
				}

				entry.clientAddrPort = clientAddrPort
			}

			entry.clientOobCache, err = conn.UpdateOobCache(entry.clientOobCache, oobBuf[:oobn], s.logger)
			if err != nil {
				s.logger.Warn("Failed to process OOB from serverConn",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", clientAddrPort),
					zap.Stringer("targetAddress", targetAddr),
					zap.Uint64("clientSessionID", csid),
					zap.Error(err),
				)
			}

			select {
			case entry.natConnSendCh <- queuedPacket{packetBufp, payloadStart, payloadLength, targetAddr}:
			default:
				s.logger.Debug("Dropping packet due to full send channel",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", clientAddrPort),
					zap.Stringer("targetAddress", targetAddr),
					zap.Uint64("clientSessionID", csid),
				)

				s.packetBufPool.Put(packetBufp)
			}

			s.mu.Unlock()
		}
	}()

	s.logger.Info("Started UDP session relay service",
		zap.String("server", s.serverName),
		zap.String("listenAddress", s.listenAddress),
		zap.Int("listenerFwmark", s.listenerFwmark),
	)

	return nil
}

func (s *UDPSessionRelay) relayServerConnToNatConnGeneric(csid uint64, entry *session) {
	// Cache the last used target address.
	//
	// When the target address is a domain, it is very likely that the target address won't change
	// throughout the lifetime of the session. In this case, caching the target address can eliminate
	// the per-packet DNS lookup overhead.
	var (
		cachedTargetAddr     socks5.Addr
		cachedTargetAddrPort netip.AddrPort = entry.natConnFixedTargetAddrPort
	)

	for {
		queuedPacket, ok := <-entry.natConnSendCh
		if !ok {
			break
		}

		packetStart, packetLength, err := entry.natConnPacker.PackInPlace(*queuedPacket.bufp, queuedPacket.targetAddr, queuedPacket.start, queuedPacket.length)
		if err != nil {
			s.logger.Warn("Failed to pack packet",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("targetAddress", queuedPacket.targetAddr),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)

			s.packetBufPool.Put(queuedPacket.bufp)
			continue
		}

		if !entry.natConnUseFixedTargetAddrPort && !bytes.Equal(cachedTargetAddr, queuedPacket.targetAddr) {
			targetAddrPort, err := queuedPacket.targetAddr.AddrPort(s.preferIPv6)
			if err != nil {
				s.logger.Warn("Failed to get target address port",
					zap.String("server", s.serverName),
					zap.String("listenAddress", s.listenAddress),
					zap.Stringer("clientAddress", entry.clientAddrPort),
					zap.Stringer("targetAddress", queuedPacket.targetAddr),
					zap.Uint64("clientSessionID", csid),
					zap.Error(err),
				)

				s.packetBufPool.Put(queuedPacket.bufp)
				continue
			}

			// Workaround for https://github.com/golang/go/issues/52264
			targetAddrPort = conn.Tov4Mappedv6(targetAddrPort)

			cachedTargetAddr = queuedPacket.targetAddr
			cachedTargetAddrPort = targetAddrPort
		}

		_, err = entry.natConn.WriteToUDPAddrPort((*queuedPacket.bufp)[packetStart:packetStart+packetLength], cachedTargetAddrPort)
		if err != nil {
			s.logger.Warn("Failed to write packet to natConn",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("targetAddress", queuedPacket.targetAddr),
				zap.Stringer("writeTargetAddress", cachedTargetAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
		}

		s.packetBufPool.Put(queuedPacket.bufp)
	}
}

func (s *UDPSessionRelay) relayNatConnToServerConnGeneric(csid uint64, entry *session) {
	serverFrontHeadroom := entry.serverConnPacker.FrontHeadroom()
	serverRearHeadroom := entry.serverConnPacker.RearHeadroom()
	clientFrontHeadroom := entry.natConnPacker.FrontHeadroom()
	clientRearHeadroom := entry.natConnPacker.RearHeadroom()

	var frontHeadroom, rearHeadroom int
	if serverFrontHeadroom > clientFrontHeadroom {
		frontHeadroom = serverFrontHeadroom - clientFrontHeadroom
	}
	if serverRearHeadroom > clientRearHeadroom {
		rearHeadroom = serverRearHeadroom - clientRearHeadroom
	}

	var (
		cachedTargetAddr         socks5.Addr
		cachedPacketFromAddrPort netip.AddrPort
	)

	packetBuf := make([]byte, frontHeadroom+entry.maxClientPacketSize+rearHeadroom)
	recvBuf := packetBuf[frontHeadroom : frontHeadroom+entry.maxClientPacketSize]

	for {
		n, _, flags, packetFromAddrPort, err := entry.natConn.ReadMsgUDPAddrPort(recvBuf, nil)
		if err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				break
			}

			s.logger.Warn("Failed to read packet from natConn",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
			continue
		}
		err = conn.ParseFlagsForError(flags)
		if err != nil {
			s.logger.Warn("Failed to read packet from natConn",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("packetFromAddress", packetFromAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
			continue
		}

		targetAddr, hasTargetAddr, payloadStart, payloadLength, err := entry.natConnUnpacker.UnpackInPlace(packetBuf, frontHeadroom, n)
		if err != nil {
			s.logger.Warn("Failed to unpack packet",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("packetFromAddress", packetFromAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Int("packetLength", n),
				zap.Error(err),
			)
			continue
		}
		if !hasTargetAddr {
			if packetFromAddrPort == cachedPacketFromAddrPort {
				targetAddr = cachedTargetAddr
			} else {
				targetAddr = socks5.AddrFromAddrPort(packetFromAddrPort)
				cachedPacketFromAddrPort = packetFromAddrPort
				cachedTargetAddr = targetAddr
			}
		}

		packetStart, packetLength, err := entry.serverConnPacker.PackInPlace(packetBuf, targetAddr, payloadStart, payloadLength)
		if err != nil {
			s.logger.Warn("Failed to pack packet",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("targetAddress", targetAddr),
				zap.Stringer("packetFromAddress", packetFromAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
			continue
		}

		_, _, err = s.serverConn.WriteMsgUDPAddrPort(packetBuf[packetStart:packetStart+packetLength], entry.clientOobCache, entry.clientAddrPort)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				break
			}

			s.logger.Warn("Failed to write packet to serverConn",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Stringer("targetAddress", targetAddr),
				zap.Stringer("packetFromAddress", packetFromAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
		}
	}
}

// Stop implements the Service Stop method.
func (s *UDPSessionRelay) Stop() error {
	if s.serverConn == nil {
		return nil
	}
	s.serverConn.Close()

	now := time.Now()

	s.mu.Lock()
	for csid, entry := range s.table {
		if err := entry.natConn.SetReadDeadline(now); err != nil {
			s.logger.Warn("Failed to set read deadline on natConn",
				zap.String("server", s.serverName),
				zap.String("listenAddress", s.listenAddress),
				zap.Stringer("clientAddress", entry.clientAddrPort),
				zap.Uint64("clientSessionID", csid),
				zap.Error(err),
			)
		}
	}
	s.mu.Unlock()

	s.wg.Wait()
	return nil
}
