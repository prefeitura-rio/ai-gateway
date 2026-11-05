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
// stub broker below.
const (
	closeStubFrameMethod = 1
	closeStubFrameEnd    = 0xCE
)

// startClosingRabbitMQStub starts a minimal AMQP 0-9-1 broker that completes
// the handshake, answers the synchronous methods the service sends on start
// (channel.open, basic.qos, exchange.declare, queue.declare, queue.bind) and on
// channel close, and answers connection.close with close-ok followed at once
// by closing the socket, as a broker may do before the client shuts down.
func startClosingRabbitMQStub(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	var connsMu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		_ = ln.Close()
		connsMu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		connsMu.Unlock()
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
			connsMu.Lock()
			conns = append(conns, conn)
			connsMu.Unlock()
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				serveClosingRabbitMQStubConn(c)
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func serveClosingRabbitMQStubConn(c net.Conn) {
	r := bufio.NewReader(c)

	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil || !bytes.Equal(header, []byte("AMQP\x00\x00\x09\x01")) {
		return
	}

	// connection.start: version 0-9, empty server properties, PLAIN, en_US.
	start := closeStubMethod(10, 10)
	start.WriteByte(0)
	start.WriteByte(9)
	closeStubLongStr(start, "")
	closeStubLongStr(start, "PLAIN")
	closeStubLongStr(start, "en_US")
	if closeStubWriteFrame(c, 0, start.Bytes()) != nil {
		return
	}

	for {
		frameType, channel, payload, err := closeStubReadFrame(r)
		if err != nil {
			return
		}
		if frameType != closeStubFrameMethod || len(payload) < 4 {
			// Heartbeats and anything else that is not a method are ignored.
			continue
		}

		classID := binary.BigEndian.Uint16(payload[0:2])
		methodID := binary.BigEndian.Uint16(payload[2:4])
		args := payload[4:]

		var reply *bytes.Buffer
		switch {
		case classID == 10 && methodID == 11: // connection.start-ok
			reply = closeStubMethod(10, 30) // connection.tune
			_ = binary.Write(reply, binary.BigEndian, uint16(2047))
			_ = binary.Write(reply, binary.BigEndian, uint32(131072))
			_ = binary.Write(reply, binary.BigEndian, uint16(0))
		case classID == 10 && methodID == 31: // connection.tune-ok
		case classID == 10 && methodID == 40: // connection.open
			reply = closeStubMethod(10, 41)
			closeStubShortStr(reply, "")
		case classID == 10 && methodID == 50: // connection.close
			// close-ok, then drop the socket right away (the deferred Close).
			_ = closeStubWriteFrame(c, channel, closeStubMethod(10, 51).Bytes())
			return
		case classID == 20 && methodID == 10: // channel.open
			reply = closeStubMethod(20, 11)
			closeStubLongStr(reply, "")
		case classID == 20 && methodID == 40: // channel.close
			reply = closeStubMethod(20, 41)
		case classID == 60 && methodID == 10: // basic.qos
			reply = closeStubMethod(60, 11)
		case classID == 40 && methodID == 10: // exchange.declare
			reply = closeStubMethod(40, 11)
		case classID == 50 && methodID == 10: // queue.declare
			reply = closeStubMethod(50, 11)
			closeStubShortStr(reply, closeStubQueueName(args))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
		case classID == 50 && methodID == 20: // queue.bind
			reply = closeStubMethod(50, 21)
		default:
			return
		}

		if reply != nil {
			if closeStubWriteFrame(c, channel, reply.Bytes()) != nil {
				return
			}
		}
	}
}

func closeStubMethod(classID, methodID uint16) *bytes.Buffer {
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, classID)
	_ = binary.Write(buf, binary.BigEndian, methodID)
	return buf
}

func closeStubShortStr(buf *bytes.Buffer, s string) {
	buf.WriteByte(byte(len(s)))
	buf.WriteString(s)
}

func closeStubLongStr(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(s)))
	buf.WriteString(s)
}

// closeStubQueueName reads the queue name of a queue.declare: a reserved
// short followed by the name as a short string.
func closeStubQueueName(args []byte) string {
	if len(args) < 3 {
		return ""
	}
	n := int(args[2])
	if len(args) < 3+n {
		return ""
	}
	return string(args[3 : 3+n])
}

func closeStubWriteFrame(w io.Writer, channel uint16, payload []byte) error {
	buf := &bytes.Buffer{}
	buf.WriteByte(closeStubFrameMethod)
	_ = binary.Write(buf, binary.BigEndian, channel)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(payload)))
	buf.Write(payload)
	buf.WriteByte(closeStubFrameEnd)
	_, err := w.Write(buf.Bytes())
	return err
}

func closeStubReadFrame(r *bufio.Reader) (byte, uint16, []byte, error) {
	head := make([]byte, 7)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, 0, nil, err
	}
	size := binary.BigEndian.Uint32(head[3:7])
	body := make([]byte, int(size)+1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, 0, nil, err
	}
	if body[size] != closeStubFrameEnd {
		return 0, 0, nil, fmt.Errorf("bad frame end %x", body[size])
	}
	return head[0], binary.BigEndian.Uint16(head[1:3]), body[:size], nil
}

// TestRabbitMQServiceClose_ReturnsWhenBrokerDropsSocketAfterCloseOk checks that
// Close returns when the broker closes the socket right after close-ok: the
// library's reader then reports the EOF on the NotifyClose channel, which
// nobody reads any more once Close has started.
func TestRabbitMQServiceClose_ReturnsWhenBrokerDropsSocketAfterCloseOk(t *testing.T) {
	const attempts = 20
	const closeTimeout = 5 * time.Second

	logger := logrus.New()
	logger.SetOutput(io.Discard)

	for i := 0; i < attempts; i++ {
		addr := startClosingRabbitMQStub(t)
		cfg := &config.Config{
			RabbitMQ: config.RabbitMQConfig{
				URL:               fmt.Sprintf("amqp://stub:stub@%s/", addr),
				Exchange:          "test_exchange",
				DLXExchange:       "test_dlx",
				UserQueue:         "test_user_queue",
				UserMessagesQueue: "test_user_messages_queue",
				MaxRetries:        0,
			},
		}

		svc, err := NewRabbitMQService(cfg, logger)
		require.NoError(t, err, "attempt %d", i+1)

		done := make(chan error, 1)
		go func() { done <- svc.Close() }()

		select {
		case err := <-done:
			// The broker may drop the socket before or after the client reads
			// close-ok; either way Close must return, and it must not fail.
			require.NoError(t, err, "attempt %d", i+1)
		case <-time.After(closeTimeout):
			t.Fatalf("attempt %d: Close did not return within %s after the broker dropped the socket following close-ok", i+1, closeTimeout)
		}
	}
}
