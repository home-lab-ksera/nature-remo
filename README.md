# Nature Remo — おうちのリモコン

Nature Remo Local API で赤外線を取得・保存・送信する日本語 Web アプリ。
Go の標準ライブラリのみで動作し、HTML/CSS/JavaScript を単一バイナリに含めます。
スマートフォンと PC に対応。複数 Remo、家電ごとのボタン、信号 JSON の登録、削除に対応します。

目的は **Nature のクラウド障害時の代替操作** です。操作に Cloud API や Nature の認証は使用しません。
必要な赤外線信号を事前に保存し、障害時はその保存済みデータだけで Remo に送信します。
Web ページ・スクリプトもアプリ内に同梱し、外部 CDN に依存しません。

```text
ブラウザー → Tailscale HTTPS Ingress → Service → Go アプリ → LAN 上の Remo:80
                                                   └→ PVC /data/buttons.json
```

## Local API の範囲

[公式仕様](https://local-swagger.nature.global/) にある `GET /messages`（最後に受信した信号）と
`POST /messages`（送信）を使います。両方に `X-Requested-With` を付け、`Expect` は付けません。
Cloud API のトークンは不要です。Nature アプリに登録済みの家電一覧や信号の自動同期、
温度・湿度・電源状態の取得は Local API にないため、このアプリでは扱いません。
送信成功は Remo の応答を意味し、家電の動作を保証しません。

[対応機種を公式情報で確認](https://developer.nature.global/en/)してください。
特に Nature Remo nano は Local API 非対応と案内されています。
Remo の IP はルーターの DHCP 予約で固定することを推奨します。
Kubernetes の通常の Pod から `.local` の mDNS は解決できるとは限らないため、固定 IP を使います。

## ローカル起動

Go 1.23 以降（コンテナと CI は Go 1.26）を使用します。

```bash
export REMO_DEVICES='[{"id":"living","name":"リビング","url":"http://192.168.1.20"}]'
LISTEN_ADDR=127.0.0.1:8080 go run .
```

`http://127.0.0.1:8080` を開きます。上記 IP は例です。
未設定の場合は接続設定待ちの画面を表示します。実機は起動時・ヘルスチェック時に操作しません。

| 環境変数 | 既定値 | 用途 |
| --- | --- | --- |
| `REMO_DEVICES` | 空配列 | `id`, `name`, `url` を持つ配列。id は英数字・`_`・`-`、最大64文字 |
| `DATA_DIR` | `./data`（コンテナ `/data`） | 信号の永続保存先 |
| `LISTEN_ADDR` | `:8080` | HTTP 待受アドレス |

`id` は保存したボタンの接続先に使うため、IP の変更時にも維持します。
`url` は管理者の環境設定のみで指定し、Web リクエストから任意の URL へ接続する API は設けません。

## ボタン登録

1. 「ボタンを登録」で Remo・家電名・ボタン名を指定します。
2. 「受信を待ち受ける」を押し、「リモコンを押してください」と表示されたら実際のリモコンを Remo に向けて押します。
3. 30秒間の待ち受け中に取得できた信号を保存します。既存の `freq`, `format`, `data` を持つ JSON も貼り付けできます。
4. 保存されたボタンを押し、家電が意図どおり動くことを確認します。

受信 API は最新信号のみ返し、受信日時を返しません。以前の信号が残っている可能性があるため、
待ち受け開始時に残っていた信号と異なるデータを取得します。完全に同じデータを再受信した場合は区別できないため、
詳細欄の「直前の信号を取得する」で内容を確認して登録できます。受信待ちは中止できます。
エアコンは「冷房 26℃・風量自動」など設定一式として登録します。
通信は5秒でタイムアウトし、送信を自動再試行しません。タイムアウト時も家電には信号が届いている可能性があります。

## コンテナ

```bash
docker build -t nature-remo:dev .
docker volume create nature-remo-data
docker run --rm --name nature-remo -p 127.0.0.1:8080:8080 \
  --read-only --cap-drop ALL --security-opt no-new-privileges \
  -e REMO_DEVICES \
  -v nature-remo-data:/data nature-remo:dev
```

非 root（UID/GID 1000）で動作します。bind mount を使う場合は `/data` を UID 1000 で書き込めるようにしてください。
書き込み先は `/data` のみです。OS パッケージやフロントエンドの npm 依存はありません。

## Kubernetes / Tailscale

既存の `home-lab-k8s` の GitOps 構成に定義を追加しています。

- workload: `manifests/apps/nature-remo/`
- Tailscale Ingress / NetworkPolicy: `manifests/access/nature-remo/`
- Argo CD: `nature-remo`（wave 12）、`nature-remo-access`（wave 13）

詳しい初回公開手順は隣接リポジトリの [docs/nature-remo.md](../home-lab-k8s/docs/nature-remo.md) を参照。
`main` への push では、テスト成功後に GitHub-hosted runner が Tailscale OIDC 経由で
`harbor.internal.qroksera.com/sandbox/nature-remo` へイメージを登録します。
タグは `build-<run-id>-<attempt>-<commit-sha>` と、公開時点の main の先頭コミットに対する `latest` です。
Actions secrets に `TS_OAUTH_CLIENT_ID`、`TS_AUDIENCE`、`HARBOR_USERNAME`、`HARBOR_PASSWORD` が必要です。
PR と手動実行はテスト・ビルドのみを行います。
初回はイメージの公開成功を確認し、`REMO_DEVICES` と NetworkPolicy の Remo IP を設定してから GitOps に反映します。
Tailscale Operator、MagicDNS、tailnet の HTTPS、対象ノードへのアクセス権が必要です。
Ingress の hostname は `nature-remo`、Service は ClusterIP です。Funnel は設定しません。

アプリの利用者認証・認可は Tailscale の ACL/grants に委ねます。許可された利用者は全ボタンの登録・削除・送信ができます。
公開インターネットや信頼できないネットワークにこのサーバーを直接公開しないでください。
NetworkPolicy は tailscale namespace からの ingress と、指定した Remo への egress に制限します。
CSRF 対策として更新 API には専用ヘッダー・JSON を要求し、別 Origin からの操作を拒否します。

## 保存・バックアップ

`/data/buttons.json` を一時ファイルへの書き込み・同期後に rename して更新します。
保存失敗時は操作を失敗として返し、メモリ上の状態も維持します。
ファイル破損時は空データで上書きせず、起動を失敗させます。
保存先は単一プロセス専用のため **replicas: 1 / Recreate** を維持してください。
PVC は Argo CD の prune/delete から保護していますが、定期バックアップは別途必要です。

バックアップは `GET /api/state` の `buttons` 配列を JSON ファイルに保存するか、更新を止めて `buttons.json` をコピーします。
復旧時はアプリを停止し、`buttons.json` を戻して UID/GID 1000・読み書き権限を確認してから再起動します。

## 障害時に使える条件

対象は Nature のクラウド障害です。Kubernetes、保存用 PVC、Remo、家庭内 LAN、および
利用端末からアプリまでの Tailscale 接続は稼働している必要があります。
この構成だけでインターネット断・Tailscale 障害・家庭内 LAN 障害まで代替できるとは限りません。
未登録の家電操作を Nature アプリの設定から障害中に復元することはできないため、必要な操作を事前に登録してください。

## 検証

```bash
make test       # Go テスト / vet / JavaScript 構文検査
make test-race  # C コンパイラがある環境で並行処理を検証（CI でも実行）
make image      # コンテナビルド
```

Go テストは HTTP の模擬 Remo を使用し、実際の家電を操作しません。
信号取得・送信、再起動後の保存、削除、CSRF、入力検証、競合、連打抑止、
通信失敗時の再送防止、redirect 拒否、保存失敗・破損を検証します。
