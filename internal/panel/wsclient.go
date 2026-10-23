// wsclient.go 极简 WebSocket 客户端——只为驱动 Chrome DevTools Protocol 而写。
//
// 为什么不引第三方库：本模块只需要「连一个 ws://、发文本、收文本」，
// 标准库 + 100 行足够；引 gobwas/gorilla 会把依赖树拖进来（本项目 go.mod 极简）。
//
// 实现范围（够用即止，明确不做的事写在这里防止误用）：
//   - 客户端握手（含 Sec-WebSocket-Key/Accept 校验）
//   - 文本帧收发；**客户端发出的帧必须掩码**（RFC 6455 硬性要求）
//   - 分片帧拼接（CDP 的大响应会分片，必须处理）
//   - 控制帧：ping→pong、close
//   - 不做：扩展协商（permessage-deflate）、二进制帧、超长帧流式处理
package panel

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"time"
)

const wsMagic = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type wsConn struct {
	conn net.Conn
	br   *bufio.Reader
}

// wsDial 连一个 ws:// URL（如 ws://127.0.0.1:9333/devtools/page/XXX）。
func wsDial(rawURL string, timeout time.Duration) (*wsConn, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	host := u.Host
	if !strings.Contains(host, ":") {
		host += ":80"
	}
	conn, err := net.DialTimeout("tcp", host, timeout)
	if err != nil {
		return nil, err
	}
	_ = conn.SetDeadline(time.Now().Add(timeout))

	keyRaw := make([]byte, 16)
	if _, err := rand.Read(keyRaw); err != nil {
		conn.Close()
		return nil, err
	}
	key := base64.StdEncoding.EncodeToString(keyRaw)
	path := u.RequestURI()
	if path == "" {
		path = "/"
	}
	req := "GET " + path + " HTTP/1.1\r\n" +
		"Host: " + u.Host + "\r\n" +
		"Upgrade: websocket\r\n" +
		"Connection: Upgrade\r\n" +
		"Sec-WebSocket-Key: " + key + "\r\n" +
		"Sec-WebSocket-Version: 13\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		conn.Close()
		return nil, err
	}
	br := bufio.NewReader(conn)
	status, err := br.ReadString('\n')
	if err != nil {
		conn.Close()
		return nil, err
	}
	if !strings.Contains(status, "101") {
		conn.Close()
		return nil, fmt.Errorf("握手失败: %s", strings.TrimSpace(status))
	}
	// 读完响应头，校验 Sec-WebSocket-Accept（防连到非 WS 服务）
	want := base64.StdEncoding.EncodeToString(sha1Sum(key + wsMagic))
	got := ""
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			conn.Close()
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			break
		}
		if k, v, ok := strings.Cut(line, ":"); ok && strings.EqualFold(strings.TrimSpace(k), "Sec-WebSocket-Accept") {
			got = strings.TrimSpace(v)
		}
	}
	if got != "" && got != want {
		conn.Close()
		return nil, fmt.Errorf("Sec-WebSocket-Accept 不匹配")
	}
	_ = conn.SetDeadline(time.Time{}) // 之后由调用方按需设 deadline
	return &wsConn{conn: conn, br: br}, nil
}

func sha1Sum(s string) []byte {
	h := sha1.New()
	h.Write([]byte(s))
	return h.Sum(nil)
}

func (c *wsConn) Close() error { return c.conn.Close() }

// writeText 发一个文本帧（客户端→服务端必须加掩码）。
func (c *wsConn) writeText(payload []byte) error {
	var head []byte
	n := len(payload)
	head = append(head, 0x81) // FIN + opcode=text
	switch {
	case n < 126:
		head = append(head, byte(n)|0x80)
	case n < 1<<16:
		head = append(head, 126|0x80)
		var b [2]byte
		binary.BigEndian.PutUint16(b[:], uint16(n))
		head = append(head, b[:]...)
	default:
		head = append(head, 127|0x80)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(n))
		head = append(head, b[:]...)
	}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	head = append(head, mask...)
	masked := make([]byte, n)
	for i := 0; i < n; i++ {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}

// readMessage 读一条完整消息（自动拼接分片、应答 ping）。
func (c *wsConn) readMessage(deadline time.Duration) ([]byte, error) {
	_ = c.conn.SetReadDeadline(time.Now().Add(deadline))
	var out []byte
	for {
		var h [2]byte
		if _, err := io.ReadFull(c.br, h[:]); err != nil {
			return nil, err
		}
		fin := h[0]&0x80 != 0
		op := h[0] & 0x0F
		masked := h[1]&0x80 != 0
		length := int64(h[1] & 0x7F)
		switch length {
		case 126:
			var b [2]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint16(b[:]))
		case 127:
			var b [8]byte
			if _, err := io.ReadFull(c.br, b[:]); err != nil {
				return nil, err
			}
			length = int64(binary.BigEndian.Uint64(b[:]))
		}
		var mask [4]byte
		if masked {
			if _, err := io.ReadFull(c.br, mask[:]); err != nil {
				return nil, err
			}
		}
		// CDP 单条消息不会超过 32MB；超了直接判异常，避免被恶意端撑爆内存。
		if length < 0 || length > 32<<20 {
			return nil, fmt.Errorf("帧长度异常: %d", length)
		}
		payload := make([]byte, length)
		if _, err := io.ReadFull(c.br, payload); err != nil {
			return nil, err
		}
		if masked {
			for i := range payload {
				payload[i] ^= mask[i%4]
			}
		}
		switch op {
		case 0x9: // ping → pong（控制帧，直接回）
			_ = c.writeControl(0xA, payload)
			continue
		case 0xA: // pong，忽略
			continue
		case 0x8: // close
			return nil, io.EOF
		case 0x0, 0x1, 0x2: // 续帧 / 文本 / 二进制
			out = append(out, payload...)
			if fin {
				return out, nil
			}
		default:
			// 未知 opcode：跳过该帧
		}
	}
}

func (c *wsConn) writeControl(op byte, payload []byte) error {
	if len(payload) > 125 {
		payload = payload[:125]
	}
	head := []byte{0x80 | op, byte(len(payload)) | 0x80}
	mask := make([]byte, 4)
	if _, err := rand.Read(mask); err != nil {
		return err
	}
	head = append(head, mask...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%4]
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.conn.Write(head); err != nil {
		return err
	}
	_, err := c.conn.Write(masked)
	return err
}
