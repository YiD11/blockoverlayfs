#!/usr/bin/env bash
# 兼容性检查：给定一个集群的 kubeconfig，找出哪些节点不满足 blockoverlayfs 的条件、不满足什么。
#
#   hack/compat-check.sh --kubeconfig <路径> [--context <名字>]
#   hack/compat-check.sh --kubeconfig <路径> --probe-image <镜像> [--namespace <命名空间>]
#
# 不带 --probe-image：只读 API，按 containerd 版本和内核版本给节点分类，不创建任何对象。
# 带 --probe-image：另外用 kubectl debug 在每个节点上建一个 debug Pod，读 NRI 是否开启和块文件相关的
# 条件（loop、ext4、snapshotter 目录、挂载传播），读完删除。镜像要节点拉得到、带 chroot，例如插件镜像。
# --namespace 是 debug Pod 所在的命名空间，默认 default。
#
# 退出码：0 没有不满足的节点，1 有，2 参数或 kubectl 出错。
set -uo pipefail

# 同时存在的 debug Pod 数，和等一批 debug Pod 结束的秒数（含拉镜像）。
BATCH=5
PROBE_TIMEOUT=120
# 插件 chart 默认的 snapshotter 目录（config.containerd.snapshotterRoot）。
DEFAULT_SNAPSHOT_DIR=/var/lib/containerd/io.containerd.snapshotter.v1.overlayfs

# ---- 参数 ----

usage="用法：$0 --kubeconfig <路径> [--context <名字>] [--probe-image <镜像> [--namespace <命名空间>]]"
KUBECONFIG_FILE="" CONTEXT="" PROBE_IMAGE="" NAMESPACE=default
while [ $# -gt 0 ]; do
	case $1 in
	--kubeconfig) KUBECONFIG_FILE=${2-} ;;
	--context) CONTEXT=${2-} ;;
	--probe-image) PROBE_IMAGE=${2-} ;;
	--namespace) NAMESPACE=${2-} ;;
	*) echo "$usage" >&2; exit 2 ;;
	esac
	shift 2 2>/dev/null || { echo "$usage" >&2; exit 2; }
done
[ -f "$KUBECONFIG_FILE" ] || { echo "$usage" >&2; exit 2; }

k() { kubectl --kubeconfig "$KUBECONFIG_FILE" ${CONTEXT:+--context "$CONTEXT"} "$@"; }

# 记一条不满足的原因、一条备注。多条之间用分号隔开。
fail() { reasons="${reasons:+$reasons；}$1"; }
note() { notes="${notes:+$notes；}$1"; }

tmp=$(mktemp -d)
mkdir "$tmp/probe"
: >"$tmp/pods"
# 退出时删掉还没删的 debug Pod。
trap 'pods=$(cat "$tmp/pods"); [ -n "$pods" ] && k -n "$NAMESPACE" delete pod $pods --wait=false >/dev/null 2>&1; rm -rf "$tmp"' EXIT
trap 'exit 2' INT TERM

# ---- 第一步：读节点，按 containerd 版本和内核版本分类 ----

# 每个节点一行：名字|Ready 条件的状态|容器运行时和版本|内核版本
NODE_FIELDS='{range .items[*]}{.metadata.name}{"|"}{.status.conditions[?(@.type=="Ready")].status}{"|"}{.status.nodeInfo.containerRuntimeVersion}{"|"}{.status.nodeInfo.kernelVersion}{"\n"}{end}'
k get nodes -o jsonpath="$NODE_FIELDS" >"$tmp/nodes" || exit 2

# 输出每个节点一行：节点|Ready|containerd 版本|内核|版本分组|备注|不满足的原因
while IFS='|' read -r node ready runtime kernel; do
	reasons="" notes=""
	runtime=${runtime:-unknown}
	version=${runtime#containerd://}
	if [ "$version" = "$runtime" ]; then
		group="非 containerd"
		fail "运行时是 $runtime，不是 containerd"
	# 版本号形如 1.7.22、v2.2.1、1.7.23-k3s2：开头是「主.次.补丁」，后面可能带后缀。
	elif [[ $version =~ ^v?([0-9]+)\.([0-9]+)\.([0-9]+)(.*)$ ]]; then
		major=${BASH_REMATCH[1]} minor=${BASH_REMATCH[2]} patch=${BASH_REMATCH[3]}
		[ -n "${BASH_REMATCH[4]}" ] && note "版本号带后缀 ${BASH_REMATCH[4]}，不是社区发布的构建"
		if [ "$major" -ge 2 ]; then
			group="2.0 及以上"
		elif [ "$minor" -lt 7 ]; then
			group="低于 1.7"
			fail "containerd $version 低于 1.7.0，没有插件使用的 NRI 接口"
		# 1.7.26 起 containerd 才把注册超时告诉插件。在更早的版本上，不含重新注册修复的插件在
		# containerd 重启后注册不回去。
		elif [ "$patch" -ge 26 ]; then
			group="1.7.26 及以上"
		else
			group="1.7.0–1.7.25"
		fi
	else
		group="无法识别"
	fi
	if [[ $kernel =~ ^([0-9]+)\.([0-9]+) ]]; then
		if [ "${BASH_REMATCH[1]}" -lt 5 ] || { [ "${BASH_REMATCH[1]}" -eq 5 ] && [ "${BASH_REMATCH[2]}" -lt 15 ]; }; then
			fail "内核 $kernel 低于 5.15"
		fi
	fi
	[ "$ready" = True ] || note "节点不是 Ready，它上报的信息可能是旧的"
	echo "$node|$ready|$version|$kernel|$group|$notes|$reasons"
done <"$tmp/nodes" >"$tmp/rows"

# ---- 第二步：在每个节点上读 NRI 和块文件相关的条件 ----

# 下面这一段在节点上执行：debug Pod 里 chroot 到节点根目录，交给节点自己的 sh。只读取，不修改。
# 每行输出「项目<TAB>值」，读不到的项值为空或者没有这一行。
# （read -d '' 把整段 heredoc 原样读进变量。）
IFS= read -r -d '' NODE_CHECK <<'EOF'
[ -e /dev/loop-control ] && loop=yes || loop=no
printf 'loop\t%s\n' "$loop"

# 已加载的 ext4 列在 /proc/filesystems 里；没有加载时看这个内核带不带 ext4 模块。
if grep -qw ext4 /proc/filesystems; then
	ext4=yes
elif grep -qs 'fs/ext4/ext4\.ko' "/lib/modules/$(uname -r)/modules.builtin" "/lib/modules/$(uname -r)/modules.dep"; then
	ext4=yes
else
	ext4=no
fi
printf 'ext4\t%s\n' "$ext4"

# containerd 的完整配置（配置文件合并默认值）。运行中的 containerd 命令行里有 --config 或 -c 时用那个文件，否则用默认的。
config=$(tr '\0' '\n' <"/proc/$(pgrep -o -x containerd)/cmdline" | awk '
	/^(--config|-c)=/ { sub(/^[^=]*=/, ""); print; exit }
	previous == "--config" || previous == "-c" { print; exit }
	{ previous = $0 }')
dump=$(containerd ${config:+--config "$config"} config dump 2>/dev/null)

# 从配置里取某一段的某个键。section 是段标题里的一截，为空表示任何段之前的顶层键。
# 去掉值的引号：1.7 输出双引号，2.x 输出单引号（\047）。
conf() {
	printf '%s\n' "$dump" | awk -v section="$1" -v key="$2" '
		BEGIN { inside = (section == "") }
		/^ *\[/ { inside = (section != "" && index($0, section) > 0); next }
		inside && $1 == key { sub(/^[^=]*= */, ""); gsub(/["\047]/, ""); print; exit }'
}

# CRI 默认的 snapshotter：2.x 在 CRI 的 images 段里，1.7 在 CRI 的 containerd 段里。
snapshotter=$(conf "cri.v1.images']" snapshotter)
[ -n "$snapshotter" ] || snapshotter=$(conf 'cri".containerd]' snapshotter)
printf 'snapshotter\t%s\n' "$snapshotter"

# overlayfs snapshotter 的目录：配置了 root_path 就是它，否则在 containerd 的 root 下。
root=$(conf "" root)
dir=$(conf snapshotter.v1.overlayfs root_path)
[ -n "$dir" ] || dir=${root:+$root/io.containerd.snapshotter.v1.overlayfs}
if [ -n "$dir" ]; then
	[ -d "$dir" ] && exists=yes || exists=no
	printf 'snapshot_dir\t%s\nsnapshot_dir_exists\t%s\n' "$dir" "$exists"

	# /proc/1/mountinfo 是节点自己的挂载表。每行第 5 列是挂载点；第 7 列起、"-" 之前是可选字段，
	# 共享挂载在其中带 shared:<组号>。取挂载点是该目录最长前缀的那一行，同一挂载点叠了几层时取最后一行。
	real=$(readlink -f "$dir" 2>/dev/null)
	awk -v dir="${real:-$dir}" '
		$5 == "/" || $5 == dir || index(dir, $5 "/") == 1 {
			if (length($5) < length(point)) next
			point = $5; propagation = "private"
			for (i = 7; i <= NF && $i != "-"; i++) if ($i ~ /^shared:/) propagation = "shared"
		}
		END { printf "propagation\t%s\nmount_point\t%s\n", propagation, point }' /proc/1/mountinfo 2>/dev/null
fi

# NRI 开启时 containerd 监听这个 socket。不能只看文件在不在：containerd 停掉后 socket 文件会留下。
# /proc/net/unix 列出内核里现有的 unix socket，第 4 列是 00010000 表示正在监听，最后一列是路径。
socket=$(conf nri.v1.nri socket_path)
socket=${socket:-/var/run/nri/nri.sock}
printf 'nri_socket\t%s\n' "$socket"
if [ -r /proc/net/unix ]; then
	awk -v path="$socket" '
		$4 == "00010000" && $NF == path { listening = "yes" }
		END { printf "nri_listening\t%s\n", (listening ? listening : "no") }' /proc/net/unix
fi
printf 'nri_disable\t%s\n' "$(conf nri.v1.nri disable)"
printf 'nri_timeout\t%s\n' "$(conf nri.v1.nri plugin_request_timeout)"

# containerd 的 root 所在的文件系统：类型和剩余空间。块文件默认放在这个目录下。
df -PT "${root:-/var/lib/containerd}" 2>/dev/null | awk 'NR == 2 { printf "fs_type\t%s\nfs_free\t%.0fGi\n", $2, $5 / 1048576 }'
exit 0
EOF

probe_aborted=""
if [ -n "$PROBE_IMAGE" ]; then
	# 探测 Ready 且第一步没有判为不满足的节点。每批 BATCH 个：一起创建，一起等，一起删。
	awk -F'|' '$2 == "True" && $7 == "" { print $1 }' "$tmp/rows" >"$tmp/to-probe"
	echo "在 $(wc -l <"$tmp/to-probe" | tr -d ' ') 个节点上各建一个 debug Pod（命名空间 $NAMESPACE，镜像 $PROBE_IMAGE），每批 $BATCH 个" >&2
	split -l "$BATCH" "$tmp/to-probe" "$tmp/batch."
	for batch in "$tmp"/batch.*; do
		[ -f "$batch" ] || break

		while read -r node; do
			# kubectl 先输出 "Creating debugging pod <名字> with container ..."，建好 Pod 就返回，不等它运行。
			# 建不出来时退出码非零，最后一行是原因。
			if created=$(k debug "node/$node" -n "$NAMESPACE" --image="$PROBE_IMAGE" -- chroot /host sh -c "$NODE_CHECK" 2>&1 </dev/null); then
				echo "$created" | sed -n 's/^Creating debugging pod \([^ ]*\) .*/\1/p' >>"$tmp/pods"
			else
				echo "$created" | tail -1 | tr '|' '/' >"$tmp/probe/$node.failed"
			fi
		done <"$batch"
		# 一整批都建不出来（多半是准入策略或权限），后面的批次也一样，不再试。
		if [ ! -s "$tmp/pods" ]; then
			probe_aborted=$(cat "$tmp/probe/$(head -1 "$batch").failed")
			break
		fi

		# 等这批 Pod 都结束，最多 PROBE_TIMEOUT 秒。拉不到镜像的不再等。每行：Pod 节点 阶段 容器在等什么
		POD_STATE='POD:.metadata.name,NODE:.spec.nodeName,PHASE:.status.phase,WAITING:.status.containerStatuses[0].state.waiting.reason'
		deadline=$((SECONDS + PROBE_TIMEOUT))
		while :; do
			k -n "$NAMESPACE" get pod $(cat "$tmp/pods") --no-headers -o custom-columns="$POD_STATE" >"$tmp/state" 2>/dev/null
			awk '($3 == "Pending" || $3 == "Running") && $4 !~ /ImagePull/ { unfinished = 1 } END { exit !unfinished }' "$tmp/state" || break
			[ "$SECONDS" -lt "$deadline" ] || break
			sleep 2
		done

		while read -r pod node phase waiting; do
			case $phase in
			Succeeded) k -n "$NAMESPACE" logs "$pod" >"$tmp/probe/$node" 2>/dev/null </dev/null ;;
			Failed)
				# 容器没起来（比如镜像里没有 chroot）时原因在 Pod 状态里，起来之后出错时在日志里。
				why=$(k -n "$NAMESPACE" get pod "$pod" -o jsonpath='{.status.containerStatuses[0].state.terminated.message}' 2>/dev/null </dev/null)
				[ -n "$why" ] || why=$(k -n "$NAMESPACE" logs "$pod" 2>&1 </dev/null | tail -1)
				echo "读取命令没有执行成功：$why" | tr '\n|' ' /' >"$tmp/probe/$node.failed"
				;;
			*)
				case $waiting in
				ErrImagePull | ImagePullBackOff) echo "节点拉不到镜像 $PROBE_IMAGE" >"$tmp/probe/$node.failed" ;;
				*) echo "debug Pod $PROBE_TIMEOUT 秒内没有结束（$phase）" >"$tmp/probe/$node.failed" ;;
				esac
				;;
			esac
		done <"$tmp/state"
		# 既没有结果也没有记下原因的节点。
		while read -r node; do
			[ -s "$tmp/probe/$node" ] || [ -e "$tmp/probe/$node.failed" ] || echo "没有取到 debug Pod 的输出" >"$tmp/probe/$node.failed"
		done <"$batch"

		k -n "$NAMESPACE" delete pod $(cat "$tmp/pods") --wait=false >/dev/null 2>&1
		: >"$tmp/pods"
	done
fi

# ---- 汇总每个节点的结论 ----

# 取当前节点探测结果里的一项。
probed() { awk -F'\t' -v key="$1" '$1 == key { print $2; exit }' "$tmp/probe/$node"; }

: >"$tmp/detail"
# 输出每个节点一行：节点|Ready|containerd 版本|内核|NRI|块文件条件|版本分组|结论|备注|不满足的原因
while IFS='|' read -r node ready version kernel group notes reasons; do
	# 这两列的取值：yes 满足，no 不满足，? 读不到，- 没有探测。
	nri="-" block="-"

	if [ -s "$tmp/probe/$node" ]; then
		loop=$(probed loop) ext4=$(probed ext4) snapshotter=$(probed snapshotter)
		dir=$(probed snapshot_dir) dir_exists=$(probed snapshot_dir_exists)
		propagation=$(probed propagation) mount_point=$(probed mount_point)
		socket=$(probed nri_socket) nri_disable=$(probed nri_disable)

		block=yes
		[ "$loop" = no ] && { block=no; fail "没有 /dev/loop-control，loop 设备不可用"; }
		[ "$ext4" = no ] && { block=no; fail "内核不支持 ext4"; }
		[ "${snapshotter:-overlayfs}" != overlayfs ] && { block=no; fail "containerd 默认的 snapshotter 是 $snapshotter，不是 overlayfs"; }
		[ "$dir_exists" = no ] && { block=no; fail "snapshotter 目录 $dir 不存在"; }
		[ "$propagation" = private ] && { block=no; fail "$dir 所在的挂载 $mount_point 不是共享挂载，插件在它下面做的挂载传不到节点"; }
		[ "${dir:-$DEFAULT_SNAPSHOT_DIR}" != "$DEFAULT_SNAPSHOT_DIR" ] && note "snapshotter 目录是 $dir，chart 的 config.containerd.snapshotterRoot 要改成它"

		unread=""
		for item in loop ext4 snapshotter snapshot_dir_exists propagation; do
			[ -n "$(probed "$item")" ] || unread="$unread $item"
		done
		if [ -n "$unread" ]; then
			note "读不到：${unread# }"
			[ "$block" = yes ] && block="?"
		fi

		case $(probed nri_listening) in
		yes) nri=yes ;;
		no)
			nri=no
			fail "NRI 没有开启：containerd 没有在监听 $socket（配置里 disable = ${nri_disable:-读不到}）"
			;;
		*) nri="?" ;;
		esac

		# 一行：读到的各项|节点和它的剩余空间。报告里把各项都相同的节点合并。
		echo "NRI disable=${nri_disable:-?} 请求超时=$(probed nri_timeout)；snapshotter=${snapshotter:-?} 目录=${dir:-?} 存在=${dir_exists:-?}；挂载点 ${mount_point:-?} ${propagation:-?}；文件系统 $(probed fs_type)；loop=${loop:-?} ext4=${ext4:-?}|$node（剩余 $(probed fs_free)）" >>"$tmp/detail"
	elif [ -f "$tmp/probe/$node.failed" ]; then
		note "没有探测成：$(cat "$tmp/probe/$node.failed")"
	fi

	if [ -n "$reasons" ]; then
		verdict="不满足"
	elif [ "$block" = "-" ]; then
		verdict="版本满足"
		[ "$group" = "无法识别" ] && verdict="版本号无法识别"
		[ -n "$PROBE_IMAGE" ] && verdict="$verdict，没有探测"
	elif [ "$block$nri" != yesyes ]; then
		verdict="部分条件读不到"
	else
		verdict="满足"
		[ "$group" = "无法识别" ] && verdict="探测的条件都满足，版本号无法识别"
	fi
	[ -z "$reasons" ] && [ "$group" = "1.7.0–1.7.25" ] && verdict="$verdict；需要含重新注册修复的插件构建"
	echo "$node|$ready|$version|$kernel|$nri|$block|$group|$verdict|$notes|$reasons"
done <"$tmp/rows" >"$tmp/final"

# ---- 报告 ----

echo "集群 $(k config view --minify -o jsonpath='{.clusters[0].cluster.server}')，节点 $(wc -l <"$tmp/final" | tr -d ' ') 个，检查时间 $(date -u +%FT%TZ)"

echo
echo "== containerd 版本分布 =="
cut -d'|' -f3,7 "$tmp/final" | sort | uniq -c | sort -rn | while read -r count row; do
	case ${row#*|} in
	"2.0 及以上") mark="" ;;
	"无法识别") mark="版本号无法识别" ;;
	"非 containerd") mark="不是 containerd" ;;
	*) mark="低于 2.0" ;;
	esac
	printf '  %4d 个节点  %s  %s\n' "$count" "${row%%|*}" "$mark"
done

echo
echo "== 逐节点（NRI、块文件两列：yes 满足，no 不满足，? 读不到，- 没有探测）=="
# 文件读两遍：第一遍量列宽，第二遍打印。
awk -F'|' '
	# 一格：文字后面补空格到列宽。shown 是文字在终端里占的列数；汉字一个占两列，所以表头的列数单独给。
	function cell(text, shown, width) { return text sprintf("%" (width - shown + 2) "s", "") }
	NR == FNR {
		if (length($1) > wn) wn = length($1)
		if (length($3) > wv) wv = length($3)
		if (length($4) > wk) wk = length($4)
		next
	}
	FNR == 1 {
		if (wn < 4) wn = 4
		if (wv < 10) wv = 10
		if (wk < 4) wk = 4
		print "  " cell("节点", 4, wn) cell("Ready", 5, 7) cell("containerd", 10, wv) cell("内核", 4, wk) cell("NRI", 3, 3) cell("块文件", 6, 6) "结论"
	}
	{ print "  " cell($1, length($1), wn) cell($2, length($2), 7) cell($3, length($3), wv) cell($4, length($4), wk) cell($5, length($5), 3) cell($6, length($6), 6) $8 }
' "$tmp/final" "$tmp/final"

echo
echo "== 不满足的节点 =="
# 节点名一行，下面每条原因一行。
awk -F'|' '
	$8 == "不满足" {
		print "  " $1
		n = split($10, why, "；")
		for (i = 1; i <= n; i++) print "    - " why[i]
		found = 1
	}
	END { if (!found) print "  （没有）" }' "$tmp/final"

echo
echo "== containerd 低于 2.0 的节点 =="
awk -F'|' -v groups="1.7.26 及以上|1.7.0–1.7.25|低于 1.7" '
	BEGIN {
		n = split(groups, order, "|")
		about["1.7.26 及以上"] = "NRI 默认关闭，要在 containerd 配置里开启"
		about["1.7.0–1.7.25"] = "NRI 默认关闭，要在 containerd 配置里开启；需要含重新注册修复的插件构建"
		about["低于 1.7"] = "不满足"
	}
	$7 in about { count[$7]++; nodes[$7] = nodes[$7] "    " $1 "  " $3 "\n"; found = 1 }
	END {
		for (i = 1; i <= n; i++) if (count[order[i]]) printf "  %s，%d 个节点（%s）\n%s", order[i], count[order[i]], about[order[i]], nodes[order[i]]
		if (!found) print "  （没有）"
	}' "$tmp/final"

if [ -s "$tmp/detail" ]; then
	echo
	echo "== 探测明细（各项都相同的节点合并成一组）=="
	awk -F'|' '
		!($1 in nodes) { order[++n] = $1 }
		{ count[$1]++; nodes[$1] = nodes[$1] "    " $2 "\n" }
		END { for (i = 1; i <= n; i++) printf "  %d 个节点：%s\n%s", count[order[i]], order[i], nodes[order[i]] }' "$tmp/detail"
fi

remarks=$(awk -F'|' '$9 != "" { print "  " $1 "：" $9 }' "$tmp/final")
if [ -n "$remarks" ]; then
	echo
	echo "== 备注 =="
	echo "$remarks"
fi

echo
[ -n "$probe_aborted" ] && echo "建不出 debug Pod，探测中止：$probe_aborted"
[ -z "$PROBE_IMAGE" ] && echo "NRI 是否开启和块文件相关的条件没有检查。加 --probe-image <镜像> 才会在节点上读取。"
if [ -n "$PROBE_IMAGE" ]; then
	unprobed=$(awk -F'|' '$8 != "不满足" && $6 != "yes"' "$tmp/final" | wc -l | tr -d ' ')
	[ "$unprobed" -eq 0 ] || echo "$unprobed 个节点没有探测或没有读全，原因见「备注」"
fi
unmet=$(awk -F'|' '$8 == "不满足"' "$tmp/final" | wc -l | tr -d ' ')
if [ "$unmet" -eq 0 ]; then
	echo "结论：没有不满足的节点"
else
	echo "结论：$unmet 个节点不满足，见「不满足的节点」"
	exit 1
fi
