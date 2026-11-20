# HeimDB のイメージ（docs/operations.md の「Docker」）。
#
#   docker buildx build --platform linux/amd64,linux/arm64 --build-arg VERSION=0.3.0 -t tsukinowa/heimdb:0.3.0 .
#
# ビルドはビルドするマシンのアーキテクチャで行い、GOARCH で目的のアーキテクチャのバイナリを作る（cgo を使わないので
# エミュレーションは要らない）。依存のライセンスの表示を /licenses に入れる（バイナリを配るので）。
#
# Go のバージョンは go.mod の toolchain の行（CI のテストが使うバージョン）と同じものにする（docs/plan.md の段階 89）。
# 記号表は残す（-s を付けない）。公開イメージのバイナリを govulncheck -mode binary で調べるとき、記号表がないと
# 呼ばない関数の脆弱性まで出る（段階 127。DWARF は -w で落とす。記号表の分で約 5 MB 大きい）。

FROM --platform=$BUILDPLATFORM golang:1.27.4 AS build
ARG TARGETOS TARGETARCH VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY LICENSE NOTICE ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags "-w -X main.version=$VERSION" -o /out/heimdb ./cmd/heimdb
RUN go install github.com/google/go-licenses/v2@v2.0.1 && \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go-licenses save ./cmd/heimdb --save_path=/out/licenses && \
    rm -rf /out/licenses/github.com/tsukinowasha
# /data と /backups は、HeimDB を動かすユーザー（nonroot）のものにする。新しい名前付きボリュームをそこに付けると、
# Docker がこの所有者を写す（/backups がないと、ボリュームが root のものになり、heimdb_backup が書けなかった）。
RUN mkdir -p /out/data /out/backups

FROM gcr.io/distroless/static-debian12:nonroot
ARG VERSION=dev
LABEL org.opencontainers.image.title="HeimDB" \
      org.opencontainers.image.description="A branchable, time-travelling MySQL-compatible database for WordPress" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.vendor="Tsukinowa Inc." \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.url="https://hub.docker.com/r/tsukinowa/heimdb"
COPY --from=build /out/heimdb /heimdb
COPY LICENSE NOTICE /
COPY --from=build /out/licenses /licenses
COPY --from=build --chown=nonroot:nonroot /out/data /data
COPY --from=build --chown=nonroot:nonroot /out/backups /backups
VOLUME /data
EXPOSE 3306
ENTRYPOINT ["/heimdb", "-addr", "0.0.0.0:3306", "-data", "/data"]
