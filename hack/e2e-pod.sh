#!/bin/sh
# The entry of the Pod that hack/e2e-run.py runs the suite in. The Pod's image has Go and make, and its
# ServiceAccount is kubectl's access to the cluster.
#
# Installs kubectl, runs make e2e in this checkout, and leaves the suite's report in /work/out for
# hack/e2e-run.py to fetch. E2E_NODE, E2E_TEST_IMAGE and E2E_LABELS are those of make e2e.
set -u
KUBECTL=v1.35.4
OUT=/work/out
mkdir -p "$OUT"
cd "$(dirname "$0")/.."

# hack/e2e-run.py copies the lines that start with "## " into its own output.
step() { echo "## $*"; }

# finish <stage> <exit code> says how far the run got, then waits for hack/e2e-run.py to have fetched /work/out,
# which goes with the Pod.
finish() {
	printf 'E2E_RESULT={"stage":"%s","exit":%s,"kubectl":"%s"}\n' "$1" "$2" "$KUBECTL"
	touch "$OUT/done"
	t=180
	while [ ! -e "$OUT/collected" ] && [ "$t" -gt 0 ]; do
		sleep 2
		t=$((t - 2))
	done
	exit "$2"
}

step "安装 kubectl $KUBECTL"
arch=$(go env GOARCH)
curl -fsSL -o /usr/local/bin/kubectl "https://dl.k8s.io/release/$KUBECTL/bin/linux/$arch/kubectl" || finish tools 1
chmod +x /usr/local/bin/kubectl

step "make e2e：编译套件，在节点 $E2E_NODE 上跑用例"
# The checkout came without its git history.
GOFLAGS=-buildvcs=false GINKGO_NO_COLOR=TRUE make e2e
rc=$?
cp _output/e2e/report.json "$OUT/" 2>/dev/null

step "make e2e 结束，退出码 $rc"
finish suite "$rc"
