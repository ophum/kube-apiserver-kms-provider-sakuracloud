# kube-apiserver-kms-plugin-sakuracloud

SAKURA Cloud KMS を利用する Kubernetes KMS v2 gRPC プラグインです。kube-apiserver の encryption at rest(保存データの暗号化)を、compose.yml だけでローカルに立ち上げた kube-apiserver + etcd 環境で動作させます。

> [!WARNING]
> **これは現在お試し実装です。本番環境での使用を意図していません。**
> KMS v2 の仕組みと SAKURA Cloud KMS の動きを学習・検証するためのデモであり、以下のような本番要件は考慮していません。
>
> - kube-apiserver が `AlwaysAllow` 認証・単一 etcd など、最小構成で動作する
> - DEK の暗号化/復号が毎回 SAKURA Cloud KMS API への同期呼び出しになるため、API のレイテンシ・レート制限・障害が Secret の読み書きに直接影響する
> - 認証情報・証明書の取り扱い、監査ログ、エラーリトライ、鍵ローテーション手順などは簡易実装
> - 暗号実装の妥当性やセキュリティは十分に検証されていない

## 仕組み

KMS v2 のエンベロープ暗号方式に従います。Secret 本体は kube-apiserver がローカル生成した DEK(32 バイト)で AES-GCM 暗号化し、プラグインが暗号化するのはその DEK のみです。DEK の暗号化/復号には SAKURA Cloud KMS の Encrypt / Decrypt API を呼び出します。

- KeyID 形式: `sakuracloud-kms:<リソースID>:v<LatestVersion>`
- プラグインは 60 秒ごとに `Key.Read` で `LatestVersion` を監視し、鍵のローテーションを検知したら KeyID を更新します
- アノテーション(`kms.sakuracloud.example.com`)に `{"key_id":..., "version":..., "algo":"aes-256-gcm"}` を記録し、復号時に鍵を特定します

## 構成

| サービス | イメージ | 役割 |
| --- | --- | --- |
| `certs` | alpine:3.20 | CA・apiserver 証明書・etcd 証明書・SA 鍵・kubectl 用 admin 証明書を `./certs/` に生成(冪等) |
| `etcd` | gcr.io/etcd-development/etcd:v3.5.16 | TLS クライアント認証付きで起動 |
| `kms-plugin` | 本リポジトリでビルド | unix ソケット(`/run/kms-plugin/socket.sock`)で KMS v2 gRPC を提供 |
| `kube-apiserver` | registry.k8s.io/kube-apiserver:v1.37.3 | `encryption-provider-config.yaml` で KMS v2 プロバイダを使用 |

6443 ポートは `127.0.0.1` にのみ公開されます。

## セットアップ

```sh
cp .env.example .env
# .env に SAKURACLOUD_ACCESS_TOKEN / SAKURACLOUD_ACCESS_TOKEN_SECRET / KMS_KEY_ID を設定

docker compose up --build -d
```

`KMS_KEY_ID` は SAKURA Cloud KMS の鍵リソース ID(例: `usacloud kms list` で確認)。未設定の場合、compose のパース時点でエラーになります。

## 動作確認

```sh
kubectl --kubeconfig certs/kubeconfig get ns

kubectl --kubeconfig certs/kubeconfig -n default create secret generic test --from-literal=foo=bar
kubectl --kubeconfig certs/kubeconfig -n default get secret test -o jsonpath='{.data.foo}' | base64 -d
```

etcd 上に平文が保存されていないことを確認:

```sh
docker compose exec -T etcd /usr/local/bin/etcdctl \
  --cacert=/certs/ca.crt \
  --cert=/certs/apiserver-etcd-client.crt \
  --key=/certs/apiserver-etcd-client.key \
  get /registry/secrets/default/test --print-value-only \
  | strings

# 平文が含まれていないことの確認
docker compose exec -T etcd /usr/local/bin/etcdctl \
  --cacert=/certs/ca.crt \
  --cert=/certs/apiserver-etcd-client.crt \
  --key=/certs/apiserver-etcd-client.key \
  get /registry/secrets/default/test --print-value-only \
  | grep -a bar && echo "PLAINTEXT LEAKED" || echo "encrypted OK"
```

`strings` の出力に `sakuracloud-kms:<リソースID>:v1` とアノテーション JSON が見えれば、KMS v2 の `EncryptedObject` として保存されています。

## 鍵のローテーション

1. `usacloud kms rotate <ID>`(または SAKURA Cloud コントロールパネル)で鍵をローテーション
2. プラグインが 60 秒以内に検知し、`KMS key version changed` を出力して以降の KeyID を `v2` に更新
3. kube-apiserver が新 KeyID を取り込み、以降の書き込みから新鍵で暗号化。既存データは書き込み時に順次再暗号化されます(読み取りは旧 KeyID のままでも復号可能)

全件を即時再暗号化する場合:

```sh
kubectl --kubeconfig certs/kubeconfig get secrets --all-namespaces \
  -o go-template='{{range .items}}{{.metadata.namespace}} {{.metadata.name}}{{"\n"}}{{end}}' \
| while read -r ns name; do
    kubectl --kubeconfig certs/kubeconfig -n "$ns" annotate secret "$name" \
      kms.sakuracloud.example.com/reencrypted=true --overwrite >/dev/null \
      && echo "done: $ns/$name"
  done
```

再暗号化結果の確認(etcd 上の KeyID 一覧):

```sh
docker compose exec -T etcd /usr/local/bin/etcdctl \
  --cacert=/certs/ca.crt \
  --cert=/certs/apiserver-etcd-client.crt \
  --key=/certs/apiserver-etcd-client.key \
  get /registry/secrets/ --prefix --print-value-only \
  | strings | grep -a sakuracloud-kms | sort -u
```

## 環境変数

| 変数 | 必須 | 説明 |
| --- | --- | --- |
| `SAKURACLOUD_ACCESS_TOKEN` | ○ | SAKURA Cloud API トークン |
| `SAKURACLOUD_ACCESS_TOKEN_SECRET` | ○ | SAKURA Cloud API シークレット |
| `KMS_KEY_ID` | ○ | 使用する鍵のリソース ID |
| `KMS_SOCKET` | - | unix ソケットのパス(デフォルト: `/run/kms-plugin/socket.sock`) |

## 注意事項

お試し実装であることの詳細は冒頭の WARNING を参照してください。その他の動作上の注意:

- デモ/検証用途の構成です。kube-apiserver は `--authorization-mode=AlwaysAllow` で動作し、6443 は localhost のみに公開しています
- アノテーションキー(`main.go` の `annotationKey`)は RFC 1123 の完全修飾ドメイン名である必要があるため、自分の管理するドメインへの変更を推奨します。キーを変えた場合は全データの再暗号化が必要です
- `KMS_KEY_ID` を別のリソース ID に変更すると、旧 ID で暗号化された既存データを復号できなくなります。ローテーションは Rotate API(`LatestVersion` の増加)を使用してください
- `immutable: true` の Secret は更新不可のため、再暗号化には削除と再作成が必要です
- 証明書を再生成する場合は `sudo rm -rf certs && docker compose up -d` を実行してください
