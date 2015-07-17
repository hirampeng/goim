package main

import (
	"bufio"
	log "code.google.com/p/log4go"
	"net"
	"time"
)

const (
	maxPackLen    = 1 << 10
	rawHeaderLen  = int16(16)
	packLenSize   = 4
	headerLenSize = 2
)

func (server *Server) serveTCP(conn *net.TCPConn, rr *bufio.Reader, wr *bufio.Writer, tr *Timer) {
	var (
		b   *Bucket
		ch  *Channel
		hb  time.Duration // heartbeat
		key string
		err error
		trd *TimerData
		p   = new(Proto)
		rpb = make([]byte, maxPackIntBuf)
		wpb = make([]byte, maxPackIntBuf) // avoid false sharing
	)
	// auth
	if trd, err = tr.Add(Conf.HandshakeTimeout, conn); err != nil {
		log.Error("handshake: timer.Add() error(%v)", err)
	} else {
		if key, hb, err = server.authTCP(rr, wr, rpb, p); err != nil {
			log.Error("handshake: server.auth error(%v)", err)
		}
		//deltimer
		tr.Del(trd)
	}
	// failed
	if err != nil {
		if err = conn.Close(); err != nil {
			log.Error("handshake: conn.Close() error(%v)", err)
		}
		return
	}
	// TODO how to reuse channel
	// register key->channel
	b = server.Bucket(key)
	ch = NewChannel(Conf.CliProto, Conf.SvrProto)
	b.Put(key, ch)
	// hanshake ok start dispatch goroutine
	go server.dispatchTCP(conn, wr, wpb, ch, hb, tr)
	for {
		// fetch a proto from channel free list
		if p, err = ch.CliProto.Set(); err != nil {
			log.Error("%s fetch client proto error(%v)", key, err)
			break
		}
		// parse request protocol
		if err = server.readTCPRequest(rr, rpb, p); err != nil {
			log.Error("%s read client request error(%v)", key, err)
			break
		}
		// send to writer
		ch.CliProto.SetAdv()
		select {
		case ch.Signal <- ProtoReady:
		default:
			log.Warn("%s send a signal, but chan is full just ignore", key)
			break
		}
	}
	// dialog finish
	// revoke the subkey
	// revoke the remote subkey
	// close the net.Conn
	// read & write goroutine
	// return channel to bucket's free list
	// may call twice
	if err = conn.Close(); err != nil {
		log.Error("reader: conn.Close() error(%v)")
	}
	// don't use close chan, Signal can be reused
	// if chan full, writer goroutine next fetch from chan will exit
	// if chan empty, send a 0(close) let the writer exit
	log.Debug("wake up dispatch goroutine")
	select {
	case ch.Signal <- ProtoFinsh:
	default:
		log.Warn("%s send proto finish signal, but chan is full just ignore", key)
	}
	b.Del(key)
	if err = server.operator.Disconnect(key); err != nil {
		log.Error("%s operator do disconnect error(%v)", key, err)
	}
	log.Debug("%s serverconn goroutine exit", key)
	return
}

// dispatch accepts connections on the listener and serves requests
// for each incoming connection.  dispatch blocks; the caller typically
// invokes it in a go statement.
func (server *Server) dispatchTCP(conn *net.TCPConn, wr *bufio.Writer, pb []byte, ch *Channel, hb time.Duration, tr *Timer) {
	var (
		p   *Proto
		err error
		trd *TimerData
		sig int
	)
	log.Debug("start dispatch goroutine")
	if trd, err = tr.Add(hb, conn); err != nil {
		log.Error("dispatch: timer.Add() error(%v)", err)
		goto failed
	}
	for {
		if sig = <-ch.Signal; sig == 0 {
			goto failed
		}
		// fetch message from clibox(client send)
		for {
			if p, err = ch.CliProto.Get(); err != nil {
				log.Debug("channel no more client message, wait signal")
				break
			}
			if p.Operation == OP_HEARTBEAT {
				// Use a previous timer value if difference between it and a new
				// value is less than TIMER_LAZY_DELAY milliseconds: this allows
				// to minimize the minheap operations for fast connections.
				if !trd.Lazy(hb) {
					tr.Del(trd)
					if trd, err = tr.Add(hb, conn); err != nil {
						log.Error("dispatch: timer.Add() error(%v)", err)
						goto failed
					}
				}
				// heartbeat
				p.Body = nil
				p.Operation = OP_HEARTBEAT_REPLY
				log.Debug("heartbeat proto: %v", p)
			} else {
				// process message
				if err = server.operator.Operate(p); err != nil {
					log.Error("operator.Operate() error(%v)", err)
					goto failed
				}
			}
			if err = server.writeTCPResponse(wr, pb, p); err != nil {
				log.Error("server.sendTCPResponse() error(%v)", err)
				goto failed
			}
			ch.CliProto.GetAdv()
		}
		// fetch message from svrbox(server send)
		for {
			if p, err = ch.SvrProto.Get(); err != nil {
				log.Debug("channel no more server message, wait signal")
				break
			}
			// just forward the message
			if err = server.writeTCPResponse(wr, pb, p); err != nil {
				log.Error("server.sendTCPResponse() error(%v)", err)
				goto failed
			}
			ch.SvrProto.GetAdv()
		}
	}
failed:
	// wake reader up
	if err = conn.Close(); err != nil {
		log.Error("conn.Close() error(%v)", err)
	}
	// deltimer
	tr.Del(trd)
	log.Debug("dispatch goroutine exit")
	return
}

// auth for goim handshake with client, use rsa & aes.
func (server *Server) authTCP(rr *bufio.Reader, wr *bufio.Writer, pb []byte, p *Proto) (subKey string, heartbeat time.Duration, err error) {
	log.Debug("get auth request protocol")
	if err = server.readTCPRequest(rr, pb, p); err != nil {
		return
	}
	if p.Operation != OP_AUTH {
		log.Warn("auth operation not valid: %d", p.Operation)
		err = ErrOperation
		return
	}
	if subKey, heartbeat, err = server.operator.Connect(p); err != nil {
		log.Error("operator.Connect error(%v)", err)
		return
	}
	log.Debug("send auth response protocol")
	p.Body = nil
	p.Operation = OP_AUTH_REPLY
	if err = server.writeTCPResponse(wr, pb, p); err != nil {
		log.Error("[%s] server.sendTCPResponse() error(%v)", subKey, err)
	}
	return
}

// readRequest
func (server *Server) readTCPRequest(rr *bufio.Reader, pb []byte, proto *Proto) (err error) {
	var (
		packLen   int32
		headerLen int16
		bodyLen   int
	)
	if err = ReadAll(rr, pb[:packLenSize]); err != nil {
		return
	}
	packLen = BigEndian.Int32(pb[:packLenSize])
	log.Debug("packLen: %d", packLen)
	if packLen > maxPackLen {
		return ErrProtoPackLen
	}
	if err = ReadAll(rr, pb[:headerLenSize]); err != nil {
		return
	}
	headerLen = BigEndian.Int16(pb[:headerLenSize])
	log.Debug("headerLen: %d", headerLen)
	if headerLen != rawHeaderLen {
		return ErrProtoHeaderLen
	}
	if err = ReadAll(rr, pb[:VerSize]); err != nil {
		return
	}
	proto.Ver = BigEndian.Int16(pb[:VerSize])
	log.Debug("protoVer: %d", proto.Ver)
	if err = ReadAll(rr, pb[:OperationSize]); err != nil {
		return
	}
	proto.Operation = BigEndian.Int32(pb[:OperationSize])
	log.Debug("operation: %d", proto.Operation)
	if err = ReadAll(rr, pb[:SeqIdSize]); err != nil {
		return
	}
	proto.SeqId = BigEndian.Int32(pb[:SeqIdSize])
	log.Debug("seqId: %d", proto.SeqId)
	bodyLen = int(packLen - int32(headerLen))
	log.Debug("read body len: %d", bodyLen)
	if bodyLen > 0 {
		proto.Body = make([]byte, bodyLen)
		if err = ReadAll(rr, proto.Body); err != nil {
			log.Error("body: ReadAll() error(%v)", err)
			return
		}
	} else {
		proto.Body = nil
	}
	return
}

// sendResponse send resp to client, sendResponse must be goroutine safe.
func (server *Server) writeTCPResponse(wr *bufio.Writer, pb []byte, proto *Proto) (err error) {
	log.Debug("write proto: %v", proto)
	BigEndian.PutInt32(pb[:packLenSize], int32(rawHeaderLen)+int32(len(proto.Body)))
	if _, err = wr.Write(pb[:packLenSize]); err != nil {
		log.Error("packLen: wr.Write() error(%v)", err)
		return
	}
	BigEndian.PutInt16(pb[:headerLenSize], rawHeaderLen)
	if _, err = wr.Write(pb[:headerLenSize]); err != nil {
		log.Error("headerLen: wr.Write() error(%v)", err)
		return
	}
	BigEndian.PutInt16(pb[:VerSize], proto.Ver)
	if _, err = wr.Write(pb[:VerSize]); err != nil {
		log.Error("protoVer: wr.Write() error(%v)", err)
		return
	}
	BigEndian.PutInt32(pb[:OperationSize], proto.Operation)
	if _, err = wr.Write(pb[:OperationSize]); err != nil {
		log.Error("operation: wr.Write() error(%v)", err)
		return
	}
	BigEndian.PutInt32(pb[:SeqIdSize], proto.SeqId)
	if _, err = wr.Write(pb[:SeqIdSize]); err != nil {
		log.Error("seqId: wr.Write() error(%v)", err)
		return
	}
	if proto.Body != nil {
		if _, err = wr.Write(proto.Body); err != nil {
			log.Error("body: wr.Write() error(%v)", err)
			return
		}
	}
	return wr.Flush()
}
