#!/bin/sh
# The entrypoint of the suite's image, test/e2e/Dockerfile, in a Pod that hack/e2e-run.py creates. The Pod's
# ServiceAccount is kubectl's access to the cluster.
#
# Runs the suite and leaves its report in /work/out for hack/e2e-run.py to fetch. E2E_NODE, E2E_TEST_IMAGE and
# E2E_SUITE_NAMESPACE are the suite's, and E2E_LABELS is a Ginkgo label filter over its groups.
set -u
OUT=/work/out
mkdir -p "$OUT"

# hack/e2e-run.py copies the lines that start with "## " into its own output.
echo "## 在节点 $E2E_NODE 上跑用例"
GINKGO_NO_COLOR=TRUE e2e.test -test.timeout 2h -ginkgo.v -ginkgo.label-filter="${E2E_LABELS:-}" \
	-ginkgo.json-report="$OUT/report.json"
rc=$?
echo "## 套件结束，退出码 $rc"

# Says how the suite ended, then waits for hack/e2e-run.py to have fetched /work/out, which goes with the Pod.
printf 'E2E_RESULT={"exit":%s,"kubectl":"%s"}\n' "$rc" "$KUBECTL_VERSION"
t=180
while [ ! -e "$OUT/collected" ] && [ "$t" -gt 0 ]; do
	sleep 2
	t=$((t - 2))
done
exit "$rc"
