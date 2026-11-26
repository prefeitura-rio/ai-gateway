package services

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/config"
)

// AMQP 0-9-1 frame type for methods and the frame terminator, as used by the
// reconnection stub broker below.
const (
	reconnStubFrameMethod = 1
	reconnStubFrameEnd    = 0xCE
)

// reconnStub is a minimal AMQP 0-9-1 broker that completes the handshake,
// answers the synchronous methods the service sends on start and on close,
// and lets the test drop every open connection to simulate a broker outage.
type reconnStub struct {
	addr string

	mu       sync.Mutex
	conns    []net.Conn
	accepted int // connections accepted so far
	ready    int // connections whose main channel and pool channels are open
}

// reconnStubChannelsPerConn is the number of channels the service opens on a
// connection: the main channel plus the five channels of the pool.
const reconnStubChannelsPerConn = 6

func startReconnStub(t *testing.T) *reconnStub {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	s := &reconnStub{addr: ln.Addr().String()}

	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
		s.dropAll()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			s.mu.Lock()
			s.conns = append(s.conns, conn)
			s.accepted++
			s.mu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				s.serve(c)
			}(conn)
		}
	}()

	return s
}

// dropAll closes every open connection from the broker side, as a broker
// restart or a network failure would.
func (s *reconnStub) dropAll() {
	s.mu.Lock()
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func (s *reconnStub) counts() (accepted, ready int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted, s.ready
}

func (s *reconnStub) serve(c net.Conn) {
	r := bufio.NewReader(c)

	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil || !bytes.Equal(header, []byte("AMQP\x00\x00\x09\x01")) {
		return
	}

	// connection.start: version 0-9, empty server properties, PLAIN, en_US.
	start := reconnStubMethod(10, 10)
	start.WriteByte(0)
	start.WriteByte(9)
	reconnStubLongStr(start, "")
	reconnStubLongStr(start, "PLAIN")
	reconnStubLongStr(start, "en_US")
	if reconnStubWriteFrame(c, 0, start.Bytes()) != nil {
		return
	}

	opened := 0
	for {
		frameType, channel, payload, err := reconnStubReadFrame(r)
		if err != nil {
			return
		}
		if frameType != reconnStubFrameMethod || len(payload) < 4 {
			// Heartbeats and anything else that is not a method are ignored.
			continue
		}

		classID := binary.BigEndian.Uint16(payload[0:2])
		methodID := binary.BigEndian.Uint16(payload[2:4])
		args := payload[4:]

		var reply *bytes.Buffer
		switch {
		case classID == 10 && methodID == 11: // connection.start-ok
			reply = reconnStubMethod(10, 30) // connection.tune
			_ = binary.Write(reply, binary.BigEndian, uint16(2047))
			_ = binary.Write(reply, binary.BigEndian, uint32(131072))
			_ = binary.Write(reply, binary.BigEndian, uint16(0))
		case classID == 10 && methodID == 31: // connection.tune-ok
		case classID == 10 && methodID == 40: // connection.open
			reply = reconnStubMethod(10, 41)
			reconnStubShortStr(reply, "")
		case classID == 10 && methodID == 50: // connection.close
			// close-ok; the client closes the socket after reading it.
			reply = reconnStubMethod(10, 51)
		case classID == 20 && methodID == 10: // channel.open
			reply = reconnStubMethod(20, 11)
			reconnStubLongStr(reply, "")
		case classID == 20 && methodID == 40: // channel.close
			reply = reconnStubMethod(20, 41)
		case classID == 60 && methodID == 10: // basic.qos
			reply = reconnStubMethod(60, 11)
			opened++
			if opened == reconnStubChannelsPerConn {
				s.mu.Lock()
				s.ready++
				s.mu.Unlock()
			}
		case classID == 40 && methodID == 10: // exchange.declare
			reply = reconnStubMethod(40, 11)
		case classID == 50 && methodID == 10: // queue.declare
			reply = reconnStubMethod(50, 11)
			reconnStubShortStr(reply, reconnStubQueueName(args))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
		case classID == 50 && methodID == 20: // queue.bind
			reply = reconnStubMethod(50, 21)
		default:
			return
		}

		if reply != nil {
			if reconnStubWriteFrame(c, channel, reply.Bytes()) != nil {
				return
			}
		}
	}
}

func reconnStubMethod(classID, methodID uint16) *bytes.Buffer {
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, classID)
	_ = binary.Write(buf, binary.BigEndian, methodID)
	return buf
}

func reconnStubShortStr(buf *bytes.Buffer, s string) {
	buf.WriteByte(byte(len(s)))
	buf.WriteString(s)
}

func reconnStubLongStr(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(s)))
	buf.WriteString(s)
}

// reconnStubQueueName reads the queue name of a queue.declare: a reserved
// short followed by the name as a short string.
func reconnStubQueueName(args []byte) string {
	if len(args) < 3 {
		return ""
	}
	n := int(args[2])
	if len(args) < 3+n {
		return ""
	}
	return string(args[3 : 3+n])
}

func reconnStubWriteFrame(w io.Writer, channel uint16, payload []byte) error {
	buf := &bytes.Buffer{}
	buf.WriteByte(reconnStubFrameMethod)
	_ = binary.Write(buf, binary.BigEndian, channel)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(payload)))
	buf.Write(payload)
	buf.WriteByte(reconnStubFrameEnd)
	_, err := w.Write(buf.Bytes())
	return err
}

func reconnStubReadFrame(r *bufio.Reader) (byte, uint16, []byte, error) {
	head := make([]byte, 7)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, 0, nil, err
	}
	size := binary.BigEndian.Uint32(head[3:7])
	body := make([]byte, int(size)+1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, 0, nil, err
	}
	if body[size] != reconnStubFrameEnd {
		return 0, 0, nil, fmt.Errorf("bad frame end %x", body[size])
	}
	return head[0], binary.BigEndian.Uint16(head[1:3]), body[:size], nil
}

// TestRabbitMQServiceReconnect_DropReconnectAndClose drives the reconnection
// loop through two broker outages and then a Close issued while the loop is
// waiting to reconnect. It checks that every outage is noticed on the
// notification channels of the current connection, and that the loop stops
// without reconnecting once Close has run. Run with -race: the loop reads the
// notification channels and the shutdown flag concurrently with connect and
// Close.
func TestRabbitMQServiceReconnect_DropReconnectAndClose(t *testing.T) {
	const waitFor = 10 * time.Second
	const tick = 20 * time.Millisecond

	logger := logrus.New()
	logger.SetOutput(io.Discard)

	stub := startReconnStub(t)
	cfg := &config.Config{
		RabbitMQ: config.RabbitMQConfig{
			URL:               fmt.Sprintf("amqp://stub:stub@%s/", stub.addr),
			Exchange:          "test_exchange",
			DLXExchange:       "test_dlx",
			UserQueue:         "test_user_queue",
			UserMessagesQueue: "test_user_messages_queue",
			MaxRetries:        3,
		},
	}

	svc, err := NewRabbitMQService(cfg, logger)
	require.NoError(t, err)

	readyAt := func(n int) func() bool {
		return func() bool {
			accepted, ready := stub.counts()
			return accepted == n && ready == n && svc.IsConnected()
		}
	}
	require.Eventually(t, readyAt(1), waitFor, tick, "initial connection not ready")

	// Two outages in a row: the second one is only noticed if the loop
	// listens on the notification channels of the connection opened by the
	// first reconnection.
	for want := 2; want <= 3; want++ {
		stub.dropAll()
		require.Eventually(t, readyAt(want), waitFor, tick, "service did not reconnect after outage %d", want-1)
	}

	// Third outage, then Close while the loop waits before reconnecting.
	stub.dropAll()
	require.Eventually(t, func() bool { return !svc.IsConnected() }, waitFor, tick, "outage not noticed")

	done := make(chan struct{})
	go func() {
		// The connection is already gone, so Close may report it; it must
		// return all the same.
		_ = svc.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(waitFor):
		t.Fatalf("Close did not return within %s", waitFor)
	}

	// The pending reconnection wakes up after its backoff and must see the
	// shutdown instead of dialing the broker again.
	require.Never(t, func() bool {
		accepted, _ := stub.counts()
		return accepted != 3
	}, 2500*time.Millisecond, tick, "service reconnected after Close")
	require.False(t, svc.IsConnected())
}
