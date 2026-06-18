#!/usr/bin/env bash
# P4.1 diag: confirm repo init + diagnose the SSD quota (find orphaned disks).
echo "=== repo init check ==="
ls -la ~/android/lineage/.repo/manifests 2>/dev/null | head -3 || echo "NO .repo"
tail -3 ~/android/lineage/.repo/manifest.xml 2>/dev/null || true
echo "=== gcloud account/project ==="
gcloud config list 2>&1 | head -12 || echo "gcloud unusable"
echo "=== all persistent disks (look for an unattached orphan) ==="
gcloud compute disks list 2>&1 | head -30
echo "=== SSD quota usage in northamerica-northeast1 ==="
gcloud compute regions describe northamerica-northeast1 2>&1 | grep -B1 -A3 SSD_TOTAL_GB | head -20
echo DIAG_DONE
