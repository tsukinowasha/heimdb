package backend

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"

	"github.com/dolthub/vitess/go/mysql"

	gmsserver "github.com/tsukinowasha/heimdb/internal/sqlengine/server"
)

// 通信の暗号化（docs/plan.md の段階 26）。

// erSecureTransportRequired は MySQL の ER_SECURE_TRANSPORT_REQUIRED（vitess に定数がない）。
const erSecureTransportRequired = 3159

// LoadTLS は、PEM の証明書と鍵を読み、MySQL の接続とレプリカへの送り出しに使う証明書を返す（Leaf も入る）。
func LoadTLS(certFile, keyFile string) (tls.Certificate, error) {
	if certFile == "" || keyFile == "" {
		return tls.Certificate{}, errors.New("HeimDB: -tls-cert and -tls-key must be given together")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("HeimDB: load TLS certificate: %w", err)
	}
	return cert, nil
}

// ListenerFactory は、go-mysql-server の既定（server.MySQLProtocolListenerFactory）と同じ受け付けを作る。
// require なら、TLS でない接続をパスワードを確かめる前にエラー 3159 で拒否する。vitess の RequireSecureTransport は、
// エラーを送った後も認証を続けるので使わない。
func ListenerFactory(require bool) gmsserver.ProtocolListenerFunc {
	return func(cfg gmsserver.Config, lc mysql.ListenerConfig, _ gmsserver.ServerEventListener) (gmsserver.ProtocolListener, error) {
		if require {
			lc.AuthServer = requireTLSAuthServer{lc.AuthServer}
		}
		l, err := mysql.NewListenerWithConfig(lc)
		if err != nil {
			return nil, err
		}
		if cfg.Version != "" {
			l.ServerVersion = cfg.Version
		}
		l.TLSConfig = cfg.TLSConfig
		return l, nil
	}
}

type requireTLSAuthServer struct{ mysql.AuthServer }

func (a requireTLSAuthServer) AuthMethods() []mysql.AuthMethod {
	methods := a.AuthServer.AuthMethods()
	out := make([]mysql.AuthMethod, len(methods))
	for i, m := range methods {
		out[i] = requireTLSMethod{m}
	}
	return out
}

// requireTLSMethod は、認証の方式の、パスワードを確かめる唯一の入り口（HandleAuthPluginData）の前に TLS を確かめる。
type requireTLSMethod struct{ mysql.AuthMethod }

func (m requireTLSMethod) HandleAuthPluginData(c *mysql.Conn, user string, serverData, clientData []byte, addr net.Addr) (mysql.Getter, error) {
	if !c.TLSEnabled() {
		// 包まずに *mysql.SQLError で返す（vitess は型で見分けて番号を送る）
		return nil, mysql.NewSQLError(erSecureTransportRequired, mysql.SSUnknownSQLState,
			"HeimDB: Connections using insecure transport are prohibited while -require-tls is set")
	}
	return m.AuthMethod.HandleAuthPluginData(c, user, serverData, clientData, addr)
}
