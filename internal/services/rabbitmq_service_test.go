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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/config"
)

// Synthetic credential used only by these tests.
const testRabbitMQPassword = "s3nh4-Sint3tica-OPS138"

// AMQP 0-9-1 frame types and the frame terminator.
const (
	amqpFrameMethod = 1
	amqpFrameEnd    = 0xCE
)

// startFakeRabbitMQ starts a minimal AMQP 0-9-1 server that completes the
// connection handshake and answers every synchronous method the gateway sends
// on start (channel.open, basic.qos, exchange.declare, queue.declare,
// queue.bind) and on close. It returns the listener address.
func startFakeRabbitMQ(t *testing.T) string {
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
				serveFakeRabbitMQConn(c)
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func serveFakeRabbitMQConn(c net.Conn) {
	r := bufio.NewReader(c)

	header := make([]byte, 8)
	if _, err := io.ReadFull(r, header); err != nil || !bytes.Equal(header, []byte("AMQP\x00\x00\x09\x01")) {
		return
	}

	// connection.start: version 0-9, empty server properties, PLAIN, en_US.
	start := amqpMethod(10, 10)
	start.WriteByte(0)
	start.WriteByte(9)
	writeLongStr(start, "")
	writeLongStr(start, "PLAIN")
	writeLongStr(start, "en_US")
	if writeFrame(c, amqpFrameMethod, 0, start.Bytes()) != nil {
		return
	}

	for {
		frameType, channel, payload, err := readFrame(r)
		if err != nil {
			return
		}
		if frameType != amqpFrameMethod || len(payload) < 4 {
			// Heartbeats and anything else that is not a method are ignored.
			continue
		}

		classID := binary.BigEndian.Uint16(payload[0:2])
		methodID := binary.BigEndian.Uint16(payload[2:4])
		args := payload[4:]

		var reply *bytes.Buffer
		switch {
		case classID == 10 && methodID == 11: // connection.start-ok
			reply = amqpMethod(10, 30) // connection.tune
			_ = binary.Write(reply, binary.BigEndian, uint16(2047))
			_ = binary.Write(reply, binary.BigEndian, uint32(131072))
			_ = binary.Write(reply, binary.BigEndian, uint16(0))
		case classID == 10 && methodID == 31: // connection.tune-ok
		case classID == 10 && methodID == 40: // connection.open
			reply = amqpMethod(10, 41)
			writeShortStr(reply, "")
		case classID == 10 && methodID == 50: // connection.close
			// The client closes the socket once it reads close-ok.
			reply = amqpMethod(10, 51)
		case classID == 20 && methodID == 10: // channel.open
			reply = amqpMethod(20, 11)
			writeLongStr(reply, "")
		case classID == 20 && methodID == 40: // channel.close
			reply = amqpMethod(20, 41)
		case classID == 60 && methodID == 10: // basic.qos
			reply = amqpMethod(60, 11)
		case classID == 40 && methodID == 10: // exchange.declare
			reply = amqpMethod(40, 11)
		case classID == 50 && methodID == 10: // queue.declare
			reply = amqpMethod(50, 11)
			writeShortStr(reply, queueNameFromDeclare(args))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
			_ = binary.Write(reply, binary.BigEndian, uint32(0))
		case classID == 50 && methodID == 20: // queue.bind
			reply = amqpMethod(50, 21)
		default:
			return
		}

		if reply != nil {
			if writeFrame(c, amqpFrameMethod, channel, reply.Bytes()) != nil {
				return
			}
		}
	}
}

func amqpMethod(classID, methodID uint16) *bytes.Buffer {
	buf := &bytes.Buffer{}
	_ = binary.Write(buf, binary.BigEndian, classID)
	_ = binary.Write(buf, binary.BigEndian, methodID)
	return buf
}

func writeShortStr(buf *bytes.Buffer, s string) {
	buf.WriteByte(byte(len(s)))
	buf.WriteString(s)
}

func writeLongStr(buf *bytes.Buffer, s string) {
	_ = binary.Write(buf, binary.BigEndian, uint32(len(s)))
	buf.WriteString(s)
}

// queueNameFromDeclare reads the queue name of a queue.declare: a reserved
// short followed by the name as a short string.
func queueNameFromDeclare(args []byte) string {
	if len(args) < 3 {
		return ""
	}
	n := int(args[2])
	if len(args) < 3+n {
		return ""
	}
	return string(args[3 : 3+n])
}

func writeFrame(w io.Writer, frameType byte, channel uint16, payload []byte) error {
	buf := &bytes.Buffer{}
	buf.WriteByte(frameType)
	_ = binary.Write(buf, binary.BigEndian, channel)
	_ = binary.Write(buf, binary.BigEndian, uint32(len(payload)))
	buf.Write(payload)
	buf.WriteByte(amqpFrameEnd)
	_, err := w.Write(buf.Bytes())
	return err
}

func readFrame(r *bufio.Reader) (byte, uint16, []byte, error) {
	head := make([]byte, 7)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0, 0, nil, err
	}
	size := binary.BigEndian.Uint32(head[3:7])
	body := make([]byte, int(size)+1)
	if _, err := io.ReadFull(r, body); err != nil {
		return 0, 0, nil, err
	}
	if body[size] != amqpFrameEnd {
		return 0, 0, nil, fmt.Errorf("bad frame end %x", body[size])
	}
	return head[0], binary.BigEndian.Uint16(head[1:3]), body[:size], nil
}

func newTestRabbitMQConfig(url string) *config.Config {
	return &config.Config{
		RabbitMQ: config.RabbitMQConfig{
			URL:               url,
			Exchange:          "test_exchange",
			DLXExchange:       "test_dlx",
			UserQueue:         "test_user_queue",
			UserMessagesQueue: "test_user_messages_queue",
			MaxRetries:        0,
		},
	}
}

func TestNewRabbitMQService_StartupLogDoesNotLeakPassword(t *testing.T) {
	addr := startFakeRabbitMQ(t)
	host, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	url := fmt.Sprintf("amqp://gateway:%s@%s/ai-gateway", testRabbitMQPassword, addr)
	logger, buf := newBufferLogger()

	svc, err := NewRabbitMQService(newTestRabbitMQConfig(url), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	out := buf.String()
	require.Contains(t, out, "RabbitMQ service initialized successfully")
	assert.NotContains(t, out, testRabbitMQPassword, "startup log must not carry the RabbitMQ password")
	assert.NotContains(t, out, url, "startup log must not carry the full RABBITMQ_URL")
	assert.Contains(t, out, `"host":"`+host+`"`, "startup log should still identify the RabbitMQ host")
	assert.Contains(t, out, `"port":`+port, "startup log should still identify the RabbitMQ port")
	assert.Contains(t, out, `"vhost":"ai-gateway"`, "startup log should still identify the RabbitMQ vhost")
}

func TestNewRabbitMQService_ParseErrorDoesNotLeakPassword(t *testing.T) {
	// The port is not numeric, so net/url refuses the URL; its error embeds
	// the whole URL unless it is redacted.
	url := fmt.Sprintf("amqp://gateway:%s@rabbitmq.invalid:porta/", testRabbitMQPassword)
	logger, buf := newBufferLogger()

	svc, err := NewRabbitMQService(newTestRabbitMQConfig(url), logger)
	require.Error(t, err)
	assert.Nil(t, svc)
	assert.Contains(t, err.Error(), "failed to connect to RabbitMQ")
	assert.NotContains(t, err.Error(), testRabbitMQPassword, "parse error must not carry the RabbitMQ password")
	assert.NotContains(t, buf.String(), testRabbitMQPassword)
}
