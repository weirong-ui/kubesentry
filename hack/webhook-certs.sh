#!/usr/bin/env bash
# webhook-certs.sh —— 不依赖 cert-manager 的 Webhook 证书方案（P3）
#
# 做三件事：
#   1. openssl 生成自签 CA + 带 SAN 的服务端证书（CN/SAN = webhook Service 域名）
#   2. 创建/更新 Secret webhook-server-cert（manager 挂载到 /tmp/k8s-webhook-server/serving-certs）
#   3. 把 CA 注入 Mutating/ValidatingWebhookConfiguration 的每个 caBundle
#
# 用法：bash hack/webhook-certs.sh [namespace] [secret 名] [service 名]
# 幂等：重复执行会重签证书并滚动更新。
set -euo pipefail

NS="${1:-kubestasis-system}"
SECRET="${2:-webhook-server-cert}"
SVC="${3:-kubestasis-webhook-service}"

TMP="$(mktemp -d)"
# 清理失败不能影响脚本退出码：某些环境（Git Bash + 安全删除钩子）会拦 rm -rf，
# 证书已经写进 Secret，临时文件删不掉只是留下垃圾，不该让整个部署链失败。
trap 'rm -rf "${TMP}" >/dev/null 2>&1 || true' EXIT

echo "==> 生成 CA 与服务端证书（CN=${SVC}.${NS}.svc）"
openssl req -x509 -newkey rsa:2048 -nodes \
  -keyout "${TMP}/ca.key" -out "${TMP}/ca.crt" -days 3650 \
  -subj "/CN=kubestasis-webhook-ca" 2>/dev/null

cat > "${TMP}/csr.conf" <<EOF
[req]
distinguished_name = dn
req_extensions = v3_req
[dn]
[v3_req]
subjectAltName = @alt_names
[alt_names]
DNS.1 = ${SVC}
DNS.2 = ${SVC}.${NS}
DNS.3 = ${SVC}.${NS}.svc
DNS.4 = ${SVC}.${NS}.svc.cluster.local
EOF

openssl req -new -newkey rsa:2048 -nodes \
  -keyout "${TMP}/tls.key" -out "${TMP}/tls.csr" \
  -subj "/CN=${SVC}.${NS}.svc" -config "${TMP}/csr.conf" 2>/dev/null

openssl x509 -req -in "${TMP}/tls.csr" \
  -CA "${TMP}/ca.crt" -CAkey "${TMP}/ca.key" -CAcreateserial \
  -out "${TMP}/tls.crt" -days 3650 \
  -extensions v3_req -extfile "${TMP}/csr.conf" 2>/dev/null

echo "==> 写入 Secret ${NS}/${SECRET}"
kubectl -n "${NS}" create secret tls "${SECRET}" \
  --cert="${TMP}/tls.crt" --key="${TMP}/tls.key" \
  --dry-run=client -o yaml | kubectl apply -f -

echo "==> 注入 caBundle"
CA_BUNDLE="$(base64 -w0 "${TMP}/ca.crt" 2>/dev/null || base64 "${TMP}/ca.crt" | tr -d '\n')"
# webhook configuration 名字由 kustomize namePrefix 决定，直接按前缀枚举更稳
for cfg in $(kubectl get mutatingwebhookconfigurations,validatingwebhookconfigurations \
    -o name | grep kubestasis); do
  count=$(kubectl get "${cfg}" -o jsonpath='{range .webhooks[*]}{.name}{"\n"}{end}' | grep -c .)
  for ((i = 0; i < count; i++)); do
    # 注意：caBundle 在 clientConfig 下面，不是 webhooks[i] 直属字段
    kubectl patch "${cfg}" --type=json \
      -p "[{\"op\":\"add\",\"path\":\"/webhooks/${i}/clientConfig/caBundle\",\"value\":\"${CA_BUNDLE}\"}]" >/dev/null
  done
  echo "    patched: ${cfg} (${count} webhook(s))"
done

echo "==> 完成。若是首次创建 Secret，需要滚动重启 manager 让它挂上证书："
echo "    kubectl -n ${NS} rollout restart deploy/kubestasis-controller-manager"
