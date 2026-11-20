package backend

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// 生のパケットのファズ（docs/plan.md の段階 105）。認証の前に壊したハンドシェイクの応答を、認証の後に壊したコマンドを
// 送る。WordPress（wpdb、mysqli）が送るのは COM_QUERY、COM_INIT_DB、COM_PING、COM_QUIT、COM_SET_OPTION などで、
// 準備した文（COM_STMT_*）は送らない。
//
//	go test ./internal/backend -run '^$' -fuzz '^FuzzPackets$' -fuzztime 30m -parallel 4

// packetMode は、入力の 1 バイト目で選ぶ送り方。
const (
	modePreAuth    = iota // ハンドシェイクの応答の代わりに data を 1 つのパケットで送る
	modePreAuthRaw        // ハンドシェイクの応答の代わりに data をそのまま送る（パケットの枠も壊す）
	modeCommand           // 認証の後に data を 1 つのコマンドのパケットで送る
	modePrepared          // fuzzPrepare を準備した（文の ID 1）後に data を 1 つのコマンドのパケットで送る
	modeCommandRaw        // 認証の後に data をそのまま送る
	packetModes
)

// fuzzPrepare は、modePrepared で準備しておく文（引数 2 つ）。
const fuzzPrepare = "SELECT ID, post_title FROM wp_posts WHERE ID = ? AND post_status = ?"

const (
	clientLongPassword   = 0x1
	clientConnectWithDB  = 0x8
	clientProtocol41     = 0x200
	clientTransactions   = 0x2000
	clientSecureConn     = 0x8000
	clientPluginAuth     = 0x80000
	fuzzClientCapability = clientLongPassword | clientConnectWithDB | clientProtocol41 | clientTransactions | clientSecureConn | clientPluginAuth
)

// rawConn は、手組みの MySQL のクライアントの接続。
type rawConn struct {
	c   net.Conn
	seq byte
}

func (r *rawConn) write(payload []byte) error {
	var h [4]byte
	h[0], h[1], h[2], h[3] = byte(len(payload)), byte(len(payload)>>8), byte(len(payload)>>16), r.seq
	r.seq++
	_, err := r.c.Write(append(h[:], payload...))
	return err
}

func (r *rawConn) read() ([]byte, error) {
	var h [4]byte
	if _, err := io.ReadFull(r.c, h[:]); err != nil {
		return nil, err
	}
	n := int(h[0]) | int(h[1])<<8 | int(h[2])<<16
	r.seq = h[3] + 1
	p := make([]byte, n)
	_, err := io.ReadFull(r.c, p)
	return p, err
}

// dialRaw は、サーバにつないで最初の挨拶を読む。
func dialRaw(addr string) (*rawConn, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	r := &rawConn{c: c}
	if _, err := r.read(); err != nil {
		c.Close()
		return nil, fmt.Errorf("read the greeting: %w", err)
	}
	return r, nil
}

// login は、パスワードのない fuzzUser として wordpress@f に入る。
func (r *rawConn) login() error {
	var b bytes.Buffer
	binary.Write(&b, binary.LittleEndian, uint32(fuzzClientCapability))
	binary.Write(&b, binary.LittleEndian, uint32(1<<24))
	b.WriteByte(255) // utf8mb4_0900_ai_ci
	b.Write(make([]byte, 23))
	b.WriteString(fuzzUser + "\x00")
	b.WriteByte(0) // 空の認証の応答
	b.WriteString(testDB + "@" + fuzzBranch + "\x00")
	b.WriteString("mysql_native_password\x00")
	if err := r.write(b.Bytes()); err != nil {
		return err
	}
	for {
		p, err := r.read()
		if err != nil {
			return err
		}
		switch {
		case len(p) > 0 && p[0] == 0x00:
			return nil
		case len(p) > 0 && p[0] == 0xfe: // 認証の方式の切り替え。空のパスワードで答える
			if err := r.write(nil); err != nil {
				return err
			}
		default:
			return fmt.Errorf("login: %q", p)
		}
	}
}

// command は、コマンドのパケットを送る（コマンドごとに番号は 0 から）。
func (r *rawConn) command(payload []byte) error {
	r.seq = 0
	return r.write(payload)
}

// prepare は fuzzPrepare を準備し、応答をすべて読む。
func (r *rawConn) prepare() error {
	if err := r.command(append([]byte{0x16}, fuzzPrepare...)); err != nil {
		return err
	}
	p, err := r.read()
	if err != nil {
		return err
	}
	if len(p) < 12 || p[0] != 0 {
		return fmt.Errorf("prepare: %q", p)
	}
	cols, params := int(binary.LittleEndian.Uint16(p[5:])), int(binary.LittleEndian.Uint16(p[7:]))
	n := 0
	if params > 0 {
		n += params + 1
	}
	if cols > 0 {
		n += cols + 1
	}
	for range n {
		if _, err := r.read(); err != nil {
			return err
		}
	}
	return nil
}

// drain は、応答を読み捨てる。50 ms 何も来なければやめる。
func (r *rawConn) drain() {
	buf := make([]byte, 4096)
	for {
		r.c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
		if _, err := r.c.Read(buf); err != nil {
			return
		}
	}
}

// runFuzzPackets は 1 つの入力を送る。サーバが拒否する（接続を閉じる）のはかまわない。
func (s *fuzzServer) runFuzzPackets(t *testing.T, mode byte, data []byte) {
	t.Helper()
	r, err := dialRaw(s.addr)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer r.c.Close()
	switch mode % packetModes {
	case modePreAuth:
		r.write(data)
	case modePreAuthRaw:
		r.c.Write(data)
	default:
		if err := r.login(); err != nil {
			t.Fatalf("login: %v", err)
		}
		if mode%packetModes == modePrepared {
			if err := r.prepare(); err != nil {
				t.Fatalf("prepare: %v", err)
			}
		}
		if mode%packetModes == modeCommandRaw {
			r.c.Write(data)
		} else {
			r.command(data)
		}
	}
	r.drain()
}

// stmtExecute は、文の ID 1 の COM_STMT_EXECUTE を作る。types は引数ごとの型（2 バイト）、values は値の並び。
func stmtExecute(nulls byte, types []byte, values []byte) []byte {
	p := []byte{0x17, 1, 0, 0, 0, 0, 1, 0, 0, 0, nulls, 1}
	return append(append(p, types...), values...)
}

// lenenc は、長さを前に付けた文字列。
func lenenc(s string) []byte { return append([]byte{byte(len(s))}, s...) }

func packetSeeds() [][2][]byte {
	le64 := func(v uint64) []byte { return binary.LittleEndian.AppendUint64(nil, v) }
	var hs bytes.Buffer
	binary.Write(&hs, binary.LittleEndian, uint32(fuzzClientCapability))
	binary.Write(&hs, binary.LittleEndian, uint32(1<<24))
	hs.WriteByte(255)
	hs.Write(make([]byte, 23))
	hs.WriteString(fuzzUser + "\x00\x00" + testDB + "@" + fuzzBranch + "\x00mysql_native_password\x00")
	seeds := [][2][]byte{
		{{modePreAuth}, hs.Bytes()},
		{{modePreAuth}, nil},
		{{modePreAuth}, []byte{0x85, 0xa6, 0xff, 0x01, 0, 0, 0, 1, 0x21}}, // SSL の要求
		{{modePreAuthRaw}, append([]byte{0xff, 0xff, 0xff, 1}, hs.Bytes()...)},
		{{modeCommand}, append([]byte{0x03}, "SELECT ID FROM wp_posts LIMIT 1"...)},
		{{modeCommand}, append([]byte{0x02}, testDB+"@"+fuzzBranch...)}, // COM_INIT_DB
		{{modeCommand}, []byte{0x0e}},                                   // COM_PING
		{{modeCommand}, []byte{0x1b, 0, 0}},                             // COM_SET_OPTION
		{{modeCommand}, append([]byte{0x04}, "wp_posts\x00%"...)},       // COM_FIELD_LIST
		{{modeCommand}, []byte{0x11, 'f', 'z', 0, 0, 0x2d, 0}},          // COM_CHANGE_USER
		{{modeCommand}, append([]byte{0x16}, fuzzPrepare...)},           // COM_STMT_PREPARE
		{{modeCommand}, []byte{0x1f}},                                   // COM_RESET_CONNECTION
		{{modeCommand}, []byte{0x0d}},                                   // COM_DEBUG
		{{modeCommand}, []byte{0x09}},                                   // COM_STATISTICS
		{{modeCommandRaw}, append([]byte{5, 0, 0, 0, 0x03}, "SELECT 1"...)},
		{{modePrepared}, stmtExecute(0, []byte{0x08, 0, 0xfd, 0}, append(le64(1), lenenc("publish")...))},
		{{modePrepared}, stmtExecute(0, []byte{0x08, 0x80, 0x05, 0}, append(le64(1<<63), le64(0x7ff8000000000001)...))},
		{{modePrepared}, stmtExecute(3, []byte{0x06, 0, 0x06, 0}, nil)},
		{{modePrepared}, stmtExecute(0, []byte{0x0c, 0, 0x0a, 0}, []byte{11, 0xe8, 0x07, 13, 32, 25, 61, 61, 0x40, 0x42, 0x0f, 0, 4, 0xff, 0xff, 1, 1})},
		{{modePrepared}, stmtExecute(0, []byte{0x0b, 0, 0xf6, 0}, append([]byte{12, 1, 0xff, 0xff, 0xff, 0xff, 25, 61, 61, 61, 0x3f, 0x42, 0x0f, 0}, lenenc("1e400")...))},
		{{modePrepared}, stmtExecute(0, []byte{0xfc, 0, 0xf7, 0}, append([]byte{0xfc, 0xff, 0xff}, "x"...))},
		{{modePrepared}, []byte{0x18, 1, 0, 0, 0, 1, 0, 'a', 'b'}}, // COM_STMT_SEND_LONG_DATA
		{{modePrepared}, []byte{0x1a, 1, 0, 0, 0}},             // COM_STMT_RESET
		{{modePrepared}, []byte{0x1c, 1, 0, 0, 0, 1, 0, 0, 0}}, // COM_STMT_FETCH
		{{modePrepared}, []byte{0x19, 2, 0, 0, 0}},             // COM_STMT_CLOSE（ない ID）
	}
	return seeds
}

// FuzzPackets は、生のパケットを送る。
func FuzzPackets(f *testing.F) {
	for _, sd := range packetSeeds() {
		f.Add(sd[0][0], sd[1])
	}
	s := startFuzzServer(f)
	f.Fuzz(func(t *testing.T, mode byte, data []byte) {
		if len(data) > 1<<16 {
			t.Skip()
		}
		defer func() {
			if t.Failed() {
				s.takePanics() // 失敗で途中に終わった入力のものを、次の入力に持ち越さない
			}
		}()
		if p := s.takePanics(); len(p) > 0 { // 前の入力の確かめの後に（切断の後片付けなどで）起きたもの
			t.Fatalf("recovered a panic after the previous input:\n%s", strings.Join(p, "\n"))
		}
		input := fmt.Sprintf("mode %d %q", mode%packetModes, data)
		watch(input, func() {
			s.runFuzzPackets(t, mode, data)
			s.check(t, input)
		})
	})
}
