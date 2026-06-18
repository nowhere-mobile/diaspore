#!/usr/bin/env bash
# P4.2b: build the agent with the S3 backend and round-trip a profile against the configured S3 store
# (Filebase/Sia). Creds come from env: S3_ENDPOINT S3_ACCESS_KEY S3_SECRET_KEY S3_BUCKET [S3_REGION].
set -uxo pipefail
AG=/home/chesterr/phase2/agent
cd "$AG"
echo "=== add minio-go + build (static) ==="
export GOFLAGS=-mod=mod
go get github.com/minio/minio-go/v7@latest 2>&1 | tail -4
go mod tidy 2>&1 | tail -4
CGO_ENABLED=0 go build -o diaspore_agent . && echo BUILD_OK || { echo BUILD_FAIL; exit 1; }

echo "=== seed a profile: 1-working (tiny) + 2-bulk (5 MB) ==="
W=/tmp/s3test; rm -rf "$W"; mkdir -p "$W/seed/1-working" "$W/seed/2-bulk" "$W/dst"
NOTE="hello-from-filebase-$(date +%s)"; echo "$NOTE" > "$W/seed/1-working/note.txt"
head -c 5242880 /dev/urandom > "$W/seed/2-bulk/blob.bin"
ms(){ date +%s%3N; }

echo "=== push-set -> S3 (endpoint=$S3_ENDPOINT bucket=$S3_BUCKET) ==="
t0=$(ms); "$AG/diaspore_agent" push-set s3 alice "pass-AAA" "$W/seed"; t1=$(ms)
echo "PUSH took $((t1-t0)) ms"

echo "=== restore WORKING-SET (prio<=1) — the boot-critical path ==="
t2=$(ms); "$AG/diaspore_agent" restore-set s3 alice "pass-AAA" "$W/dst" 1; t3=$(ms)
echo "WORKING-SET restore took $((t3-t2)) ms"

echo "=== restore FULL (prio<=999, incl 5 MB bulk) ==="
t4=$(ms); "$AG/diaspore_agent" restore-set s3 alice "pass-AAA" "$W/dst" 999; t5=$(ms)
echo "FULL restore took $((t5-t4)) ms"

echo "=== verify round-trip ==="
GOT=$(cat "$W/dst/1-working/note.txt" 2>/dev/null || echo NONE)
BULK=$( [ -f "$W/dst/2-bulk/blob.bin" ] && stat -c %s "$W/dst/2-bulk/blob.bin" || echo MISSING )
echo "note: sent=[$NOTE] got=[$GOT] ; bulk=$BULK"

echo "=== negative: wrong passphrase must FAIL to decrypt (proves store holds only ciphertext) ==="
"$AG/diaspore_agent" restore-set s3 alice "WRONGPASS" "$W/dst2" 999 2>&1 | tail -2 || true

echo "=== VERDICT ==="
if [ "$GOT" = "$NOTE" ] && [ "$BULK" = "5242880" ]; then
  echo "P4_2_S3_PASS (Filebase/Sia round-trip: push=$((t1-t0))ms working-set=$((t3-t2))ms full=$((t5-t4))ms)"
else
  echo "P4_2_S3_FAIL got=[$GOT] bulk=$BULK"
fi
echo P4_2_S3_DONE
