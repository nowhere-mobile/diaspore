#!/usr/bin/env bash
# P4.2b: seed a test profile into the S3 store (new key) + provision the baked device config.
# Creds from env (S3_ENDPOINT S3_REGION S3_BUCKET S3_ACCESS_KEY S3_SECRET_KEY).
set -uxo pipefail
SRC=/mnt/build/lineage
AG=/home/chesterr/phase2/agent
VD=$SRC/vendor/diaspore
PROFILE=alice; PASS=pass-AAA

echo "=== seed profile '$PROFILE' -> $S3_ENDPOINT/$S3_BUCKET (new key) ==="
W=/tmp/provseed; rm -rf "$W" /tmp/provverify; mkdir -p "$W/seed/1-home" "$W/seed/2-media"
echo "Diaspore on FP3 — restored from Sia at $(date)" > "$W/seed/1-home/welcome.txt"
printf 'theme=dark\nlang=en\n' > "$W/seed/1-home/settings.txt"
head -c 1048576 /dev/urandom > "$W/seed/2-media/photo.bin"
"$AG/diaspore_agent" push-set s3 "$PROFILE" "$PASS" "$W/seed"

echo "=== verify round-trip (working set) ==="
"$AG/diaspore_agent" restore-set s3 "$PROFILE" "$PASS" /tmp/provverify 1
SEED=$( [ -f /tmp/provverify/1-home/welcome.txt ] && echo SEED_OK || echo SEED_FAIL )
echo "$SEED"; cat /tmp/provverify/1-home/welcome.txt 2>/dev/null || true

echo "=== provision baked device config (creds, NOT committed) ==="
mkdir -p "$VD/etc"
cat > "$VD/etc/diaspore.conf" <<EOF
S3_ENDPOINT=$S3_ENDPOINT
S3_REGION=$S3_REGION
S3_BUCKET=$S3_BUCKET
S3_ACCESS_KEY=$S3_ACCESS_KEY
S3_SECRET_KEY=$S3_SECRET_KEY
DIASPORE_PROFILE=$PROFILE
DIASPORE_PASS=$PASS
EOF
chmod 600 "$VD/etc/diaspore.conf"
ls -la "$VD/etc/diaspore.conf"
echo "RESULT=$SEED"
echo P4_2_PROVISION_DONE
