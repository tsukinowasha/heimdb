// HeimDB は、WordPress 専用のブランチ型データベースを MySQL 互換のサーバとして起動する。
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tsukinowasha/heimdb/internal/sqlengine/server"
	"github.com/tsukinowasha/heimdb/internal/sqlengine/sql"

	"github.com/tsukinowasha/heimdb/internal/backend"
	"github.com/tsukinowasha/heimdb/internal/metrics"
	"github.com/tsukinowasha/heimdb/internal/replication"
	"github.com/tsukinowasha/heimdb/internal/store"
)

// serverVersion は、接続のときに名乗る MySQL のバージョン（互換の目標）。
const serverVersion = "8.0.33"

// version は HeimDB のバージョン（イメージを建てるときに -ldflags "-X main.version=..." で入れる）。
var version = "dev"

func main() {
	// MySQL のサーバ（RDS などの既定）と同じく UTC で動かす（docs/plan.md の段階 17）。go-mysql-server は TIMESTAMP の
	// 値をセッションの time_zone で変換せずにそのまま持つので、UTC 以外で動かすと、NOW() で入った値や mysqldump
	// （SET TIME_ZONE='+00:00' で書き出す）で移した値がずれる。system_time_zone の既定値はパッケージの初期化で
	// 決まるので、ここで入れ直す。
	time.Local = time.UTC
	if err := sql.SystemVariables.AssignValues(map[string]interface{}{
		"system_time_zone": "UTC",
		// バージョンの番号は互換の目標の MySQL のバージョン（WordPress の has_cap や mysqldump の /*!40101 ... */ がこれで判断する）。
		// go-mysql-server の既定は、接続のときに名乗るバージョン（vitess の 8.0.33）と @@version（8.0.31）が食い違い、
		// 製品名（@@version_comment）が Dolt になっている。
		"version":         serverVersion,
		"version_comment": "HeimDB",
		// サーバのファイルを読み書きする文は構文解析の直後に拒否する（docs/plan.md の段階 31）。二重の守りとして、
		// go-mysql-server の確かめる場所を、ディレクトリになりえないパスにする（空ならどのファイルでも読み書きする）。
		"secure_file_priv": "/dev/null/heimdb",
		// 照合順序の既定を MySQL 8.0 と同じにする（docs/plan.md の段階 90）。go-mysql-server の既定は utf8mb4_0900_bin。
		// 接続の照合順序は、接続のときにクライアントが伝えたものにする（backend.handshakeCollation）。
		"collation_server":     "utf8mb4_0900_ai_ci",
		"collation_connection": "utf8mb4_0900_ai_ci",
		"collation_database":   "utf8mb4_0900_ai_ci",
	}); err != nil {
		logrus.Fatalf("system variables: %v", err)
	}
	// Docker コンテナから接続できるよう、既定では全インターフェースで待ち受ける。
	addr := flag.String("addr", "0.0.0.0:3306", "listen address")
	dataDir := flag.String("data", "data", "Pebble data directory")
	dbName := flag.String("db", "wordpress", "database name served from the main branch")
	debug := flag.Bool("debug", false, "log every query (logrus debug level)")
	lockWait := flag.Duration("lock-wait-timeout", store.DefaultLockTimeout, "how long a write waits for the write lock (like innodb_lock_wait_timeout)")
	idleLock := flag.Duration("idle-lock-timeout", 30*time.Second, "close a connection idle in a transaction that has held a branch's write lock this long (0 disables)")
	retention := flag.Duration("retention", 30*24*time.Hour, "how long old versions are kept for AS OF and restore (0 keeps them forever)")
	gcInterval := flag.Duration("gc-interval", time.Hour, "how often old versions are removed")
	rootPassword := flag.String("root-password", "", "password of root@'%', created on the first start (or set HEIMDB_ROOT_PASSWORD)")
	siteUser := flag.String("site-user", "wordpress", "user for WordPress, created with privileges on the -db database only when -site-password is set and it does not exist yet")
	sitePassword := flag.String("site-password", "", "password of the -site-user (or set HEIMDB_SITE_PASSWORD)")
	backupDir := flag.String("backup-dir", "", "directory where CALL heimdb_backup() writes backups (disabled if empty); put it on the same filesystem as -data, as all writes pause while a backup is taken")
	shutdownTimeout := flag.Duration("shutdown-timeout", 30*time.Second, "how long to wait for running statements on SIGTERM before exiting without closing the store")
	slowQuery := flag.Duration("slow-query", 0, "log statements that take longer than this (0 disables; change it without a restart with SET GLOBAL long_query_time and slow_query_log)")
	slowQueryRedact := flag.Bool("slow-query-redact", false, "replace the values in logged slow statements with ?")
	noAuth := flag.Bool("no-auth", false, "accept any user without a password (development only)")
	cacheSize := flag.Int64("cache-size", 0, "size of the block cache in bytes (like innodb_buffer_pool_size); 0 uses 1/8 of the memory limit, between 16 MiB and 128 MiB (128 MiB without a limit)")
	memoryLimit := flag.String("memory-limit", "", "memory limit of the process, like 4G or 512M (or set HEIMDB_MEMORY_LIMIT); statements near it fail with error 1041 instead of the process running out of memory; empty uses the container's cgroup limit, 0 disables")
	replAddr := flag.String("replication-addr", "", "listen address for replicas (HTTP, or HTTPS with -tls-cert; disabled if empty)")
	replToken := flag.String("replication-token", "", "shared secret between the primary and replicas (or set HEIMDB_REPLICATION_TOKEN)")
	logRetention := flag.Duration("replication-log-retention", store.DefaultLogRetention, "how long commit records are kept for replicas (0 keeps none)")
	syncTimeout := flag.Duration("replication-sync-timeout", 10*time.Second, "how long a commit waits for a replica to apply it before the primary switches to async (0 never waits)")
	replicateFrom := flag.String("replicate-from", "", "run as a read-only replica of the primary at host:port or https://host:port (its -replication-addr); -data must be a backup of the primary")
	hostname, _ := os.Hostname()
	replicaName := flag.String("replica-name", hostname, "name of this replica shown in the primary's heimdb_status")
	tlsCert := flag.String("tls-cert", "", "PEM certificate for TLS on MySQL connections and the replication listener (HTTPS); restart to replace it")
	tlsKey := flag.String("tls-key", "", "PEM private key of -tls-cert")
	requireTLS := flag.Bool("require-tls", false, "reject MySQL connections that do not use TLS (needs -tls-cert)")
	replCA := flag.String("replication-ca", "", "PEM CA certificates that verify the primary's certificate (makes -replicate-from https; the system roots are used otherwise)")
	metricsAddr := flag.String("metrics-addr", "", "listen address for Prometheus metrics at /metrics (HTTP without authentication; disabled if empty), like 127.0.0.1:9104; do not expose it outside the host or network")
	var backupKeys [][2]string
	flag.Func("backup-key", "name=path: let CALL heimdb_backup('<backup>', '<name>') write a backup encrypted to the age recipients (age1..., one per line) in the file at path, read at every backup (repeat for more keys; needs -backup-dir)", func(v string) error {
		name, path, ok := strings.Cut(v, "=")
		if !ok || name == "" || path == "" {
			return fmt.Errorf("want name=path")
		}
		backupKeys = append(backupKeys, [2]string{name, path})
		return nil
	})
	resetPasswords := flag.Bool("reset-passwords", false, "at startup, set the password of root@'%' to -root-password, and of an existing -site-user to -site-password, keeping the other users (for a store restored from a backup; or set HEIMDB_RESET_PASSWORDS=1)")
	resetUsers := flag.Bool("reset-users", false, "at startup, drop every user and create root@'%' (and the -site-user with -site-password) again (for a store restored from a backup; or set HEIMDB_RESET_USERS=1)")
	rebuildIndexes := flag.Bool("rebuild-indexes", true, "after startup, rebuild in the background the indexes on main that an older HeimDB made without entries (DECIMAL, FLOAT, DOUBLE and ENUM columns from 0.2.38 or earlier, SET and BIT from 0.2.40 or earlier), one at a time; -rebuild-indexes=false leaves them to CALL heimdb_rebuild_indexes() (or set HEIMDB_REBUILD_INDEXES, used only when the flag is not given)")
	branchProcs := flag.Bool("branch-procedures", false, "let users with the EXECUTE privilege on a branch's database call heimdb_branch_diff, heimdb_branch_resolve, heimdb_changes, heimdb_restore_preview and heimdb_restore on that branch (or set HEIMDB_BRANCH_PROCEDURES=1, used only when the flag is not given); the diff shows what changed on the parent")
	flag.Parse()

	// サブコマンド（docs/plan.md の段階 87）。Docker のイメージの ENTRYPOINT は -addr と -data を渡すので、その後ろに
	// 続く（docker run <image> restore-backup ...）。
	if flag.NArg() > 0 {
		switch flag.Arg(0) {
		case "restore-backup":
			if err := restoreBackup(flag.Args()[1:], *dataDir); err != nil {
				fmt.Fprintf(os.Stderr, "heimdb restore-backup: %v\n", err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stderr, "heimdb restore-backup: restored into %s\n", *dataDir)
			return
		default:
			logrus.Fatalf("unknown command %q", flag.Arg(0))
		}
	}
	for _, f := range []struct {
		v   *bool
		env string
	}{{resetPasswords, "HEIMDB_RESET_PASSWORDS"}, {resetUsers, "HEIMDB_RESET_USERS"}, {rebuildIndexes, "HEIMDB_REBUILD_INDEXES"}} {
		name := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(f.env, "HEIMDB_"), "_", "-"))
		if !isFlagSet(name) && os.Getenv(f.env) != "" { // フラグで明示すれば、環境変数より優先する
			b, err := strconv.ParseBool(os.Getenv(f.env))
			if err != nil {
				logrus.Fatalf("%s: %v", f.env, err)
			}
			*f.v = b
		}
	}
	if *resetPasswords || *resetUsers {
		switch {
		case *resetPasswords && *resetUsers:
			logrus.Fatalf("give either -reset-passwords or -reset-users")
		case *replicateFrom != "":
			logrus.Fatalf("-reset-passwords and -reset-users cannot be used on a replica (its users come from the primary)")
		case *noAuth:
			logrus.Fatalf("-reset-passwords and -reset-users cannot be used with -no-auth")
		}
	}
	if len(backupKeys) > 0 && *backupDir == "" {
		logrus.Fatalf("-backup-key needs -backup-dir")
	}

	if *debug {
		// 受信したクエリが「Starting query」行として接続 ID 付きで出力される。
		logrus.SetLevel(logrus.DebugLevel)
	}

	if *replToken == "" {
		*replToken = os.Getenv("HEIMDB_REPLICATION_TOKEN") // -help に出さないため、既定値にはしない
	}
	if *replAddr == "" && isFlagSet("replication-sync-timeout") {
		logrus.Fatalf("-replication-sync-timeout needs -replication-addr")
	}
	if *metricsAddr != "" && *metricsAddr == *replAddr {
		logrus.Fatalf("-metrics-addr must differ from -replication-addr")
	}
	if (*replAddr != "" || *replicateFrom != "") && *replToken == "" {
		logrus.Fatalf("replication needs a shared secret: set -replication-token or HEIMDB_REPLICATION_TOKEN")
	}
	// 通信の暗号化（docs/plan.md の段階 26）。ストアを開く前に、設定の誤りで止める。
	var cert *tls.Certificate
	if *tlsCert != "" || *tlsKey != "" {
		c, err := backend.LoadTLS(*tlsCert, *tlsKey)
		if err != nil {
			logrus.Fatal(err)
		}
		cert = &c
		logrus.Infof("TLS certificate for %s, valid until %s", certNames(c.Leaf), c.Leaf.NotAfter.UTC().Format(time.RFC3339))
		// 起動は止めない（WordPress は証明書を確かめないので動く。確かめるクライアントとレプリカは失敗する）
		if now := time.Now(); now.After(c.Leaf.NotAfter) {
			logrus.Warnf("the TLS certificate expired at %s; clients and replicas that verify it will fail", c.Leaf.NotAfter.UTC().Format(time.RFC3339))
		} else if now.Before(c.Leaf.NotBefore) {
			logrus.Warnf("the TLS certificate is not valid until %s; clients and replicas that verify it will fail", c.Leaf.NotBefore.UTC().Format(time.RFC3339))
		}
	}
	if *requireTLS && cert == nil {
		logrus.Fatalf("-require-tls needs -tls-cert and -tls-key")
	}
	var rootCAs *x509.CertPool
	if *replCA != "" {
		if *replicateFrom == "" {
			logrus.Fatalf("-replication-ca needs -replicate-from")
		}
		pem, err := os.ReadFile(*replCA)
		if err != nil {
			logrus.Fatalf("replication CA: %v", err)
		}
		rootCAs = x509.NewCertPool()
		if !rootCAs.AppendCertsFromPEM(pem) {
			logrus.Fatalf("replication CA: no certificates in %s", *replCA)
		}
	}
	if *replicateFrom != "" {
		if _, err := replication.Source(*replicateFrom, rootCAs != nil); err != nil {
			logrus.Fatal(err)
		}
	}
	if *memoryLimit == "" {
		*memoryLimit = os.Getenv("HEIMDB_MEMORY_LIMIT")
	}
	memLimit, err := backend.ParseMemoryLimit(*memoryLimit)
	if err != nil {
		logrus.Fatal(err)
	}
	backend.SetMemoryLimit(memLimit)
	autoCache := *cacheSize <= 0
	if autoCache {
		*cacheSize = backend.AutoCacheSize(memLimit, store.DefaultCacheSize)
	}
	if memLimit > 0 {
		logrus.Infof("HeimDB memory limit: %d MiB (block cache %d MiB)", memLimit>>20, *cacheSize>>20)
	}
	st, err := store.OpenWith(*dataDir, store.Options{CacheSize: *cacheSize, LogRetention: *logRetention, NoLog: *logRetention == 0, Replica: *replicateFrom != ""})
	if err != nil {
		logrus.Fatalf("open store: %v", err)
	}
	st.LockTimeout = *lockWait
	// 上限が、開いた直後の分とキャッシュに足りなければ、どの文も拒否され続けるので起動しない（docs/plan.md の段階 58）。
	if err := backend.CheckStartupMemory(*cacheSize, autoCache); err != nil {
		st.Close()
		logrus.Fatal(err)
	}

	pro := backend.NewProvider(st, *dbName)
	pro.Retention = *retention
	pro.Version = version
	if err := backend.SetSlowQuery(*slowQuery); err != nil {
		logrus.Fatalf("-slow-query: %v", err)
	}
	pro.SlowQueryRedact = *slowQueryRedact
	branchProcsSet := false
	flag.Visit(func(f *flag.Flag) { branchProcsSet = branchProcsSet || f.Name == "branch-procedures" })
	if !branchProcsSet && os.Getenv("HEIMDB_BRANCH_PROCEDURES") != "" { // フラグで明示すれば、環境変数より優先する
		if *branchProcs, err = strconv.ParseBool(os.Getenv("HEIMDB_BRANCH_PROCEDURES")); err != nil {
			logrus.Fatalf("HEIMDB_BRANCH_PROCEDURES: %v", err)
		}
	}
	pro.BranchProcedures = *branchProcs
	if *backupDir != "" {
		// 結果のパスを接続元でも意味のあるものにする。
		abs, err := filepath.Abs(*backupDir)
		if err != nil {
			logrus.Fatalf("backup dir: %v", err)
		}
		if err := checkBackupDir(*dataDir, abs); err != nil {
			logrus.Fatal(err)
		}
		pro.BackupDir = abs
		pro.CleanBackupDir()
		for _, k := range backupKeys {
			if err := pro.AddBackupKey(k[0], k[1]); err != nil {
				logrus.Fatalf("-backup-key: %v", err)
			}
		}
	}
	if *retention > 0 && *gcInterval > 0 {
		go runGC(st, *retention, *gcInterval)
	}
	if st.Logging() {
		go runTrimLog(st, *gcInterval)
	}
	engine := backend.NewEngine(pro)
	if *rootPassword == "" {
		*rootPassword = os.Getenv("HEIMDB_ROOT_PASSWORD") // -help に出さないため、既定値にはしない
	}
	if *noAuth {
		logrus.Warn("authentication is disabled (-no-auth)")
		if users, err := st.Users(); err == nil && len(users) > 0 {
			logrus.Warn("users exist, but -no-auth lets anyone read and write every branch without a password")
		}
	} else if created, err := backend.EnableAuth(engine, st, *rootPassword); err != nil {
		logrus.Fatalf("auth: %v", err)
	} else if !created && *rootPassword != "" && !*resetPasswords && !*resetUsers {
		// root のパスワードは最初の起動でだけ使う。変えるには ALTER USER を使う。
		logrus.Warn("users already exist, so the root password was ignored (change it with ALTER USER, or start with -reset-passwords)")
	}
	// WordPress は root でつながず、本番のデータベースだけの権限のユーザーでつなぐ（docs/plan.md の段階 32）。
	// レプリカのユーザーは本番から写る。
	if *sitePassword == "" {
		*sitePassword = os.Getenv("HEIMDB_SITE_PASSWORD")
	}
	// 退避から復元した store を、本番のパスワードを知らない環境で開く（docs/plan.md の段階 87）。
	if *resetPasswords || *resetUsers {
		flagName := "-reset-passwords"
		if *resetUsers {
			flagName = "-reset-users"
		}
		changed, kept, err := backend.ResetUsers(engine, *rootPassword, *siteUser, *sitePassword, *resetUsers)
		if err != nil {
			logrus.Fatalf("%s: %v", flagName, err)
		}
		// 何をしたか（したことになっているか）。書き換えがなかったときも同じ形で出す。
		var what []string
		what = append(what, "root@'%'")
		if *sitePassword != "" {
			what = append(what, fmt.Sprintf("%s@'%%' (created with privileges on %s.* if missing)", *siteUser, *dbName))
		}
		did := "set the passwords of " + strings.Join(what, " and ")
		if *resetUsers {
			did += ", and dropped every other user"
		}
		if changed {
			logrus.Warnf("%s: %s (remove the flag once the store is set up; on a primary this also reaches the replicas)", flagName, did)
		} else {
			already := "the passwords were already as given"
			if *resetUsers {
				already += " and no other user existed"
			}
			logrus.Warnf("%s: nothing to change (%s)", flagName, already)
		}
		if len(kept) > 0 {
			logrus.Warnf("%s: kept %s with the passwords from before; drop them, or use -reset-users", flagName, strings.Join(kept, ", "))
		}
	}
	if *sitePassword != "" {
		switch {
		case *noAuth:
			logrus.Warn("-site-password is ignored with -no-auth")
		case st.IsReplica():
		default:
			if created, err := backend.EnsureSiteUser(engine, pro, *siteUser, *sitePassword, *dbName); err != nil {
				logrus.Fatalf("auth: %v", err)
			} else if created {
				logrus.Infof("created the site user %s@'%%' with privileges on %s.* only", *siteUser, *dbName)
			}
		}
	}
	// レプリケーション（docs/plan.md の段階 24）。止めるときは、ストアを閉じる前に受け取りと送り出しを止める。
	var replicaDone chan struct{}
	stopReplica := func() {}
	if *replicateFrom != "" {
		rep, err := replication.NewReplica(st, *replicateFrom, *replToken, *replicaName, rootCAs)
		if err != nil {
			logrus.Fatalf("replication: %v", err)
		}
		pro.ReplicationStatus, pro.ReplicationMetrics = rep.Status, rep.WriteMetrics
		ctx, cancel := context.WithCancel(context.Background())
		replicaDone = make(chan struct{})
		go func() {
			defer close(replicaDone)
			rep.Run(ctx)
		}()
		stopReplica = func() { cancel(); <-replicaDone }
		logrus.Infof("replicating from %s as %q (read-only)", *replicateFrom, *replicaName)
	}
	var replSrv *http.Server
	var prim *replication.Primary
	if *replAddr != "" {
		if !st.Logging() {
			logrus.Fatalf("-replication-addr needs commit records (-replication-log-retention must not be 0)")
		}
		prim = replication.NewPrimary(st, *replToken, *syncTimeout)
		if pro.ReplicationStatus == nil {
			pro.ReplicationStatus, pro.ReplicationMetrics = prim.Status, prim.WriteMetrics
		}
		ln, err := net.Listen("tcp", *replAddr)
		if err != nil {
			logrus.Fatalf("replication listen: %v", err)
		}
		replSrv = &http.Server{Handler: prim}
		scheme := "http"
		if cert != nil {
			// MySQL とは別の設定にする（ServeTLS が HTTP/2 の設定を足す）
			replSrv.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
			// TLS なしと同じ HTTP/1.1 にする（ServeTLS は既定で HTTP/2 を足す）
			replSrv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
			scheme = "https"
		}
		go func() {
			var err error
			if cert != nil {
				err = replSrv.ServeTLS(ln, "", "")
			} else {
				err = replSrv.Serve(ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				logrus.Errorf("replication server: %v", err)
			}
		}()
		logrus.Infof("serving replicas on %s://%s", scheme, *replAddr)
	}

	// 指標の口（docs/plan.md の段階 73）。認証がないので、外に開けない。
	var metricsSrv *http.Server
	if *metricsAddr != "" {
		ln, err := net.Listen("tcp", *metricsAddr)
		if err != nil {
			logrus.Fatalf("metrics listen: %v", err)
		}
		metricsSrv = &http.Server{Handler: metrics.Handler(pro.WriteMetrics),
			ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second}
		go func() {
			if err := metricsSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logrus.Errorf("metrics server: %v", err)
			}
		}()
		logrus.Infof("serving metrics on http://%s/metrics", *metricsAddr)
	}

	cfg := server.Config{Protocol: "tcp", Address: *addr, Version: serverVersion,
		ProtocolListenerFactory: backend.ListenerFactory(*requireTLS)}
	pro.TLS = "off"
	if cert != nil {
		cfg.TLSConfig = &tls.Config{Certificates: []tls.Certificate{*cert}, MinVersion: tls.VersionTLS12}
		pro.TLS, pro.TLSNotAfter = "on", cert.Leaf.NotAfter
		if *requireTLS {
			pro.TLS = "required"
		}
	}
	srv, err := server.NewServer(cfg, engine, sql.NewContext, backend.NewSessionBuilder(pro), nil)
	if err != nil {
		logrus.Fatalf("create server: %v", err)
	}
	pro.Processes, pro.KillConnection, pro.IdleLockTimeout = engine.ProcessList.Processes, srv.SessionManager().KillConnection, *idleLock

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
		<-sig
		// 2 回目のシグナルはすぐに終える（既定の動作に戻す。コミット済みのデータは同期書き込み済みなので安全）。
		signal.Reset(os.Interrupt, syscall.SIGTERM)
		logrus.Info("shutting down (send the signal again to exit immediately)")
		if err := srv.Close(); err != nil {
			logrus.Errorf("close server: %v", err)
		}
	}()

	logrus.Infof("HeimDB listening on %s (database %q, data %s)", *addr, *dbName, *dataDir)
	pro.WarmStats()
	pro.WatchIdleLocks()
	// 古い HeimDB で作ったエントリのないインデックスを、本番の main で裏で作り直す（docs/plan.md の段階 121）。
	pro.StartIndexRebuild(*rebuildIndexes)
	if err := srv.Start(); err != nil {
		logrus.Errorf("server: %v", err)
	}
	// 新しい文を拒否し、実行中の文が終わってから閉じる（docs/plan.md の段階 18）。待ちきれなければ閉じずに終える
	// （コミット済みのデータは同期書き込み済みなので、クラッシュと同じで安全。閉じると実行中の文が panic する）。
	// 本番は、最後のコミットまでレプリカに送ってから送り出しを閉じる（段階 25）。実行中の文と AUTO_INCREMENT の保存の
	// コミットも知らせを待ち、その後はコミットしない。
	stopReplica()
	if !pro.Shutdown(*shutdownTimeout) {
		logrus.Errorf("statements still running after %s; exiting without closing the store (committed data is safe)", *shutdownTimeout)
		os.Exit(1)
	}
	if err := st.FlushAutoIncrements(); err != nil {
		logrus.Errorf("save AUTO_INCREMENT counters: %v", err)
	}
	if metricsSrv != nil {
		// 読んでいる途中の指標が閉じたストアに触らないよう、終わるのを待ってから閉じる。
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := metricsSrv.Shutdown(ctx); err != nil {
			metricsSrv.Close()
		}
		cancel()
	}
	if replSrv != nil {
		prim.Finish()
		replSrv.Close()
		prim.Close()
	}
	logrus.Infof("last commit %s", time.UnixMicro(int64(st.Written())).UTC().Format("2006-01-02 15:04:05.000000"))
	if err := st.Close(); err != nil {
		logrus.Fatalf("close store: %v", err)
	}
	logrus.Info("stopped")
}

// certNames は、証明書の名前（SAN の DNS 名と IP。なければ CN）を返す。
func certNames(c *x509.Certificate) string {
	var names []string
	names = append(names, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		names = append(names, ip.String())
	}
	if len(names) == 0 {
		return c.Subject.CommonName
	}
	return strings.Join(names, ", ")
}

// isFlagSet は、コマンドラインでフラグ name を指定したかを返す。
// checkBackupDir は、-backup-dir の場所を確かめる。-data そのものは、バックアップの名前によっては Pebble が自分の
// ファイルと読み違えるので拒否する。-data の中のディレクトリ（Docker で 1 つのボリュームに置き、ハードリンクで済ませる
// ため。docs/stages/131.md）は、-data の直下の名前が Pebble のファイルの名前になりえないときだけ許す。Pebble は、
// 読めない名前のものを開くときも消すときも飛ばす（pebble の base.ParseFilename）。
func checkBackupDir(dataDir, backupDir string) error {
	data, err := filepath.Abs(dataDir)
	if err != nil {
		return fmt.Errorf("-data: %w", err)
	}
	rel, err := filepath.Rel(data, backupDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil // -data の外
	}
	if rel == "." {
		return errors.New("-backup-dir must differ from -data")
	}
	top, _, _ := strings.Cut(rel, string(filepath.Separator))
	if strings.Contains(top, ".") || top == "LOCK" || strings.HasPrefix(top, "MANIFEST-") || strings.HasPrefix(top, "OPTIONS-") {
		return fmt.Errorf("-backup-dir inside -data must be under a directory whose name has no '.' and is not a store file name (got %q)", top)
	}
	return nil
}

func isFlagSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) { set = set || f.Name == name })
	return set
}

// runTrimLog は、間隔 interval ごと（0 なら 1 時間ごと）に、保持期間を過ぎたコミットの記録を消す（docs/plan.md の段階 24）。
func runTrimLog(st *store.Store, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	for range time.Tick(interval) {
		if n, err := st.TrimLog(); err != nil {
			logrus.Errorf("trim replication log: %v", err)
		} else if n > 0 {
			logrus.Infof("trimmed %d commit records", n)
		}
	}
}

// runGC は、間隔 interval ごとに、保持期間 retention を過ぎた古いバージョンを消す（docs/plan.md の段階 9）。
func runGC(st *store.Store, retention, interval time.Duration) {
	for range time.Tick(interval) {
		start := time.Now()
		stats, err := st.GC(retention)
		if err != nil {
			logrus.Errorf("gc: %v", err)
			continue
		}
		logrus.Infof("gc: removed %d versions, %d tables, %d indexes, %d merge records before %s in %s",
			stats.Versions, stats.Tables, stats.Indexes, stats.MergeRecords,
			time.UnixMicro(int64(stats.Boundary)).UTC().Format(time.RFC3339), time.Since(start).Round(time.Millisecond))
		// 開いたままのトランザクション（や使用中の時刻）が境界を止めていたら知らせる。
		if held := time.Duration(stats.Target-stats.Boundary) * time.Microsecond; stats.Target > stats.Boundary && held > interval {
			logrus.Warnf("gc: the boundary is held back by %s by an open transaction (an idle connection inside BEGIN?)", held.Round(time.Second))
		}
	}
}
