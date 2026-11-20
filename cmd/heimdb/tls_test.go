package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"
)

// 通信の暗号化（docs/plan.md の段階 26）。

// testCA は、テスト用の CA。
type testCA struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	file string // PEM
}

func newTestCA(t *testing.T, dir, name string) *testCA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := &testCA{cert: cert, key: key, file: filepath.Join(dir, name+"-ca.pem")}
	writePEM(t, ca.file, "CERTIFICATE", der)
	return ca
}

func (ca *testCA) pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.cert)
	return p
}

// issue は、localhost と 127.0.0.1 の証明書と鍵を作り、そのファイルを返す。
func (ca *testCA) issue(t *testing.T, dir, name string) (certFile, keyFile string) {
	t.Helper()
	return ca.issueFor(t, dir, name, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

// issueFor は、有効な期間が [notBefore, notAfter] の証明書と鍵を作る。
func (ca *testCA) issueFor(t *testing.T, dir, name string, notBefore, notAfter time.Time) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: notBefore, NotAfter: notAfter,
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		t.Fatal(err)
	}
	kder, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile = filepath.Join(dir, name+".pem"), filepath.Join(dir, name+"-key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	writePEM(t, keyFile, "EC PRIVATE KEY", kder)
	return certFile, keyFile
}

func writePEM(t *testing.T, file, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

var tlsConfigs atomic.Int64

// tlsDB は、TLS で接続する（ca で証明書を、serverName が空でなければその名前を確かめる）。
func (s *proc) tlsDB(t *testing.T, user string, ca *testCA, serverName string) *sql.DB {
	t.Helper()
	name := "heimdb" + time.Now().Format("150405") + string(rune('a'+tlsConfigs.Add(1)%26)) + t.Name()
	name = strings.NewReplacer("/", "_").Replace(name)
	if err := mysql.RegisterTLSConfig(name, &tls.Config{RootCAs: ca.pool(), ServerName: serverName}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("mysql", user+"@tcp("+s.addr+")/wordpress?tls="+name)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func plainDB(t *testing.T, s *proc, user string) *sql.DB {
	t.Helper()
	db, err := sql.Open("mysql", user+"@tcp("+s.addr+")/wordpress")
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func pingErrNo(db *sql.DB) (uint16, error) {
	defer db.Close()
	err := db.Ping()
	return mysqlErrNo(err), err
}

// TLS を受け付けて必須にしないときは、暗号化しない接続も使える。TLS の接続は証明書とホスト名を確かめられる。
func TestTLSOffered(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "test")
	other := newTestCA(t, dir, "other")
	cert, key := ca.issue(t, dir, "server")
	srv := startArgs(t, bin, "-data", t.TempDir(), "-root-password", "pw", "-tls-cert", cert, "-tls-key", key)
	defer stopProc(srv)

	plain := plainDB(t, srv, "root:pw")
	defer plain.Close()
	if v := statusValue(t, plain, "connection_tls"); v != "no" {
		t.Errorf("plain connection_tls: %q", v)
	}
	if v := statusValue(t, plain, "tls"); v != "on" {
		t.Errorf("tls: %q", v)
	}
	if v := statusValue(t, plain, "tls_cert_not_after"); v == "" {
		t.Error("no tls_cert_not_after")
	}
	for _, name := range []string{"", "localhost"} {
		db := srv.tlsDB(t, "root:pw", ca, name)
		if v := statusValue(t, db, "connection_tls"); v != "yes" {
			t.Errorf("TLS (server name %q) connection_tls: %q", name, v)
		}
		db.Close()
	}
	// 違う CA、違う名前は、クライアントが拒否する
	if _, err := pingErrNo(srv.tlsDB(t, "root:pw", other, "")); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("other CA: %v", err)
	}
	if _, err := pingErrNo(srv.tlsDB(t, "root:pw", ca, "db.example.com")); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Errorf("other name: %v", err)
	}
	if !strings.Contains(srv.log.String(), "TLS certificate for localhost, 127.0.0.1") {
		t.Errorf("log:\n%s", srv.log)
	}
}

// -require-tls なら、暗号化しない接続はパスワードを確かめる前に 3159 で拒否する（-no-auth でも）。
func TestRequireTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "test")
	cert, key := ca.issue(t, dir, "server")
	for _, c := range []struct {
		name string
		args []string
		user string
	}{{"auth", []string{"-root-password", "pw"}, "root:pw"}, {"no-auth", []string{"-no-auth"}, "anyone"}} {
		t.Run(c.name, func(t *testing.T) {
			srv := startArgs(t, bin, append([]string{"-data", t.TempDir(), "-tls-cert", cert, "-tls-key", key, "-require-tls"}, c.args...)...)
			defer stopProc(srv)
			for _, user := range []string{c.user, "root:wrong"} {
				if n, err := pingErrNo(plainDB(t, srv, user)); n != 3159 || !strings.Contains(err.Error(), "HeimDB: ") {
					t.Errorf("plain %s: %d %v", user, n, err)
				}
			}
			db := srv.tlsDB(t, c.user, ca, "")
			defer db.Close()
			if v := statusValue(t, db, "tls"); v != "required" {
				t.Errorf("tls: %q", v)
			}
			if n, _ := pingErrNo(srv.tlsDB(t, "root:wrong", ca, "")); c.name == "auth" && n != 1045 {
				t.Errorf("TLS with a wrong password: %d", n)
			}
		})
	}
}

// 設定の誤りは、起動でエラーにする。
func TestTLSStartupErrors(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "test")
	cert, key := ca.issue(t, dir, "server")
	_, key2 := ca.issue(t, dir, "server2")
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-require-tls"}, "-require-tls needs -tls-cert"},
		{[]string{"-tls-cert", cert}, "must be given together"},
		{[]string{"-tls-key", key}, "must be given together"},
		{[]string{"-tls-cert", filepath.Join(dir, "none.pem"), "-tls-key", key}, "no such file"},
		{[]string{"-tls-cert", cert, "-tls-key", key2}, "private key does not match"},
		{[]string{"-replication-ca", ca.file}, "-replication-ca needs -replicate-from"},
		{[]string{"-replication-ca", cert + ".none", "-replicate-from", "127.0.0.1:1", "-replication-token", "x"}, "no such file"},
		{[]string{"-replication-ca", ca.file, "-replicate-from", "http://127.0.0.1:1", "-replication-token", "x"}, "needs an https:// source"},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		args := append([]string{"-addr", freeAddr(t), "-data", t.TempDir(), "-root-password", "pw"}, c.args...)
		out, err := exec.CommandContext(ctx, bin, args...).CombinedOutput()
		cancel()
		if err == nil || !strings.Contains(string(out), c.want) {
			t.Errorf("%v: %v\n%s", c.args, err, out)
		}
	}
}

// 期限切れか、まだ有効でない証明書でも起動するが（WordPress は確かめない）、警告を出す。
func TestTLSCertValidityWarning(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "test")
	for _, c := range []struct {
		name     string
		from, to time.Time
		want     string
	}{
		{"expired", time.Now().Add(-48 * time.Hour), time.Now().Add(-24 * time.Hour), "the TLS certificate expired"},
		{"future", time.Now().Add(24 * time.Hour), time.Now().Add(48 * time.Hour), "the TLS certificate is not valid until"},
		{"valid", time.Now().Add(-time.Hour), time.Now().Add(time.Hour), ""},
	} {
		cert, key := ca.issueFor(t, dir, c.name, c.from, c.to)
		srv := startArgs(t, bin, "-data", t.TempDir(), "-root-password", "pw", "-tls-cert", cert, "-tls-key", key)
		db := plainDB(t, srv, "root:pw")
		if v := statusValue(t, db, "tls"); v != "on" {
			t.Errorf("%s: tls %q", c.name, v)
		}
		db.Close()
		stopProc(srv)
		log := srv.log.String()
		if c.want != "" && !strings.Contains(log, c.want) || c.want == "" && strings.Contains(log, "the TLS certificate") {
			t.Errorf("%s: log:\n%s", c.name, log)
		}
	}
}

// レプリカは HTTPS で追いつく。違う CA では止まらずにエラーを出し、正しい CA で起動し直すと続きから追いつく。
// 暗号化しないで HTTPS の本番につなぐと、わかるエラーになる。
func TestReplicationTLS(t *testing.T) {
	if testing.Short() {
		t.Skip("starts heimdb processes")
	}
	bin := heimdbBinary(t)
	dir := t.TempDir()
	ca := newTestCA(t, dir, "test")
	other := newTestCA(t, dir, "other")
	cert, key := ca.issue(t, dir, "server")
	replAddr := freeAddr(t)
	primary := startArgs(t, bin, "-data", t.TempDir(), "-backup-dir", t.TempDir(), "-root-password", "pw",
		"-tls-cert", cert, "-tls-key", key, "-replication-addr", replAddr, "-replication-token", replToken)
	defer stopProc(primary)
	pdb := primary.db(t)
	defer pdb.Close()
	mustExec(t, pdb, "CREATE TABLE t (id bigint unsigned NOT NULL AUTO_INCREMENT PRIMARY KEY, v int)")
	var backupDir, at string
	var size int64
	if err := pdb.QueryRow("CALL heimdb_backup('replica')").Scan(&backupDir, &at, &size); err != nil {
		t.Fatal(err)
	}
	mustExec(t, pdb, "INSERT INTO t (v) VALUES (1)")
	if !strings.Contains(primary.log.String(), "serving replicas on https://") {
		t.Errorf("primary log:\n%s", primary.log)
	}
	// TLS なしと同じ HTTP/1.1（HTTP/2 を申し出ても選ばない）
	conn, err := tls.Dial("tcp", replAddr, &tls.Config{RootCAs: ca.pool(), NextProtos: []string{"h2", "http/1.1"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := conn.ConnectionState().NegotiatedProtocol; p == "h2" {
		t.Errorf("replication negotiated %q", p)
	}
	conn.Close()

	waitError := func(replica *proc, want string) {
		t.Helper()
		rdb := replica.db(t)
		defer rdb.Close()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(statusValue(t, rdb, "replication_last_error"), want) {
			if time.Now().After(deadline) {
				t.Fatalf("no %q error:\n%s\n%s", want, queryAll(t, rdb, "CALL heimdb_status()"), replica.log)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if s := statusValue(t, rdb, "replication_state"); s != "running" {
			t.Errorf("state: %s", s)
		}
	}
	base := []string{"-data", backupDir, "-replication-token", replToken, "-replica-name", "r1"}
	replica := startArgs(t, bin, append(base, "-replicate-from", replAddr)...)
	waitError(replica, "HTTP request to an HTTPS server")
	stopProc(replica)
	replica = startArgs(t, bin, append(base, "-replicate-from", "https://"+replAddr, "-replication-ca", other.file)...)
	waitError(replica, "certificate")
	stopProc(replica)

	// 方式のない本番も、CA があれば https
	replica = startArgs(t, bin, append(base, "-replicate-from", replAddr, "-replication-ca", ca.file)...)
	defer stopProc(replica)
	rdb := replica.db(t)
	defer rdb.Close()
	waitCaughtUp(t, pdb, rdb, replica)
	mustExec(t, pdb, "INSERT INTO t (v) VALUES (2)")
	waitCaughtUp(t, pdb, rdb, replica)
	if n := queryAll(t, rdb, "SELECT COUNT(*) FROM t"); strings.TrimSpace(n) != "2" {
		t.Errorf("rows on the replica: %s", n)
	}
}
