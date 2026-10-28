package services

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/prefeitura-rio/app-eai-agent-gateway/internal/config"
)

// Synthetic credential used only by these tests.
const testRedisPassword = "s3nh4-Sint3tica-OPS43"

// startFakeRedis starts a minimal RESP server that accepts the handshake
// go-redis performs on connect (HELLO is refused so the client falls back to
// AUTH + SELECT) and answers PING. It returns the listener address.
func startFakeRedis(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	var wg sync.WaitGroup
	t.Cleanup(func() {
		_ = ln.Close()
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
			wg.Add(1)
			go func(c net.Conn) {
				defer wg.Done()
				defer func() { _ = c.Close() }()
				serveFakeRedisConn(c)
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func serveFakeRedisConn(c net.Conn) {
	r := bufio.NewReader(c)
	for {
		args, err := readRESPCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}

		var reply string
		switch strings.ToUpper(args[0]) {
		case "HELLO":
			reply = "-ERR unknown command 'HELLO'\r\n"
		case "PING":
			reply = "+PONG\r\n"
		default:
			reply = "+OK\r\n"
		}
		if _, err := io.WriteString(c, reply); err != nil {
			return
		}
	}
}

func readRESPCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected RESP line: %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}

	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		header, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		header = strings.TrimRight(header, "\r\n")
		if !strings.HasPrefix(header, "$") {
			return nil, fmt.Errorf("unexpected RESP bulk header: %q", header)
		}
		size, err := strconv.Atoi(header[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:size]))
	}
	return args, nil
}

func newTestRedisConfig(dsn string) *config.Config {
	return &config.Config{
		Redis: config.RedisConfig{
			DSN:                dsn,
			PoolSize:           2,
			MinIdleConnections: 0,
			MaxIdleConnections: 1,
			DialTimeout:        2,
			ReadTimeout:        2,
			WriteTimeout:       2,
		},
	}
}

func newBufferLogger() (*logrus.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	logger := logrus.New()
	logger.SetOutput(&buf)
	logger.SetLevel(logrus.DebugLevel)
	logger.SetFormatter(&logrus.JSONFormatter{})
	return logger, &buf
}

func TestNewRedisService_StartupLogDoesNotLeakPassword(t *testing.T) {
	addr := startFakeRedis(t)
	dsn := fmt.Sprintf("redis://default:%s@%s/3", testRedisPassword, addr)
	logger, buf := newBufferLogger()

	svc, err := NewRedisService(newTestRedisConfig(dsn), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = svc.Close() })

	out := buf.String()
	require.Contains(t, out, "Redis service initialized successfully")
	assert.NotContains(t, out, testRedisPassword, "startup log must not carry the Redis password")
	assert.NotContains(t, out, dsn, "startup log must not carry the full DSN")
	assert.Contains(t, out, `"addr":"`+addr+`"`, "startup log should still identify the Redis address")
	assert.Contains(t, out, `"db":3`, "startup log should still identify the Redis database")
}

func TestNewRedisService_ParseErrorDoesNotLeakPassword(t *testing.T) {
	// The port is not numeric, so net/url refuses the DSN; its error embeds
	// the whole URL unless it is redacted.
	dsn := fmt.Sprintf("redis://default:%s@redis.invalid:porta/0", testRedisPassword)
	logger, buf := newBufferLogger()

	svc, err := NewRedisService(newTestRedisConfig(dsn), logger)
	require.Error(t, err)
	assert.Nil(t, svc)
	assert.Contains(t, err.Error(), "failed to parse Redis DSN")
	assert.NotContains(t, err.Error(), testRedisPassword, "parse error must not carry the Redis password")
	assert.NotContains(t, buf.String(), testRedisPassword)
}
