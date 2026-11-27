#!/bin/sh
set -eu

cd /certs

if [ -f ca.crt ] && [ -f apiserver.crt ] && [ -f etcd-server.crt ]; then
  echo "certificates already exist, skipping generation"
  exit 0
fi

echo "generating certificates..."

openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days 3650 -subj "/CN=demo-kube-ca" -out ca.crt

openssl genrsa -out apiserver.key 2048
openssl req -new -key apiserver.key -subj "/CN=kube-apiserver" -out apiserver.csr
cat > apiserver.ext <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:kube-apiserver,DNS:localhost,IP:127.0.0.1
EOF
openssl x509 -req -in apiserver.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -sha256 -extfile apiserver.ext -out apiserver.crt

openssl genrsa -out etcd-server.key 2048
openssl req -new -key etcd-server.key -subj "/CN=etcd" -out etcd-server.csr
cat > etcd-server.ext <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=DNS:etcd,DNS:localhost,IP:127.0.0.1
EOF
openssl x509 -req -in etcd-server.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -sha256 -extfile etcd-server.ext -out etcd-server.crt

openssl genrsa -out apiserver-etcd-client.key 2048
openssl req -new -key apiserver-etcd-client.key -subj "/CN=kube-apiserver-etcd-client" -out apiserver-etcd-client.csr
cat > client.ext <<'EOF'
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=clientAuth
EOF
openssl x509 -req -in apiserver-etcd-client.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -sha256 -extfile client.ext -out apiserver-etcd-client.crt

openssl genrsa -out sa.key 2048
openssl rsa -in sa.key -pubout -out sa.pub

openssl genrsa -out admin.key 2048
openssl req -new -key admin.key -subj "/O=system:masters/CN=kubernetes-admin" -out admin.csr
openssl x509 -req -in admin.csr -CA ca.crt -CAkey ca.key -CAcreateserial -days 3650 -sha256 -extfile client.ext -out admin.crt

cat > kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters:
- name: demo
  cluster:
    server: https://127.0.0.1:6443
    certificate-authority-data: $(openssl base64 -A -in ca.crt)
users:
- name: admin
  user:
    client-certificate-data: $(openssl base64 -A -in admin.crt)
    client-key-data: $(openssl base64 -A -in admin.key)
contexts:
- name: demo
  context:
    cluster: demo
    user: admin
current-context: demo
EOF

chmod 600 ./*.key
chmod 644 ./kubeconfig
rm -f ./*.csr ./*.ext
echo "certificates generated"
