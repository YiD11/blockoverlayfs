#!/usr/bin/env python3
"""Runs the e2e suite against the plugin a cluster runs, on nodes of the cluster kubectl points at, and reports on
the run.

    hack/e2e-run.py run --out PREFIX
    hack/e2e-run.py render [--summary] PREFIX.json

run creates a Pod of the suite's image, test/e2e/Dockerfile, which runs the suite (hack/e2e-pod.sh), with a
ServiceAccount that may do what the suite does, RULES, for as long as the run takes. It runs the groups of specs at
once, each on a node of its own and in a Pod of its own, or all in one Pod if it has one node to run on. It follows
the Pods to their end, fetches the suite's reports, deletes what the run created, and writes three files: PREFIX.md,
a report to read; PREFIX.json, the same as data; PREFIX.log, the Pods' output. It exits 0 if the run allows a
release and 1 if it does not. It needs python3 and kubectl, and nothing else of the repository: the image has this
script too.

run deploys no plugin: the cluster is to run one, ready, on every node the run takes. The specs leave it, containerd
and the node's other Pods alone, and no Pod of the run has a privilege on its node.

render prints the report of a PREFIX.json again, or the summary of it. It needs no cluster.

run reads its settings from the environment:

    E2E_IMAGE          the suite's image
    E2E_NODE           the nodes to test on, separated by commas; or
    E2E_NODE_SELECTOR  a label selector: run picks at random among the ready, schedulable nodes it selects that run
                       the plugin
    E2E_TEST_IMAGE     the image of the suite's own Pods; busybox:1.36 beside E2E_IMAGE if empty
    E2E_NAMESPACE      the namespace of the Pods that run the suite; default if empty
"""
import argparse
import datetime
import json
import os
import pathlib
import random
import re
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time

GROUPS = ('basic', 'load')
# The groups, from the one that takes longest: the order in which run deals them out.
LONGEST_FIRST = ('load', 'basic')
# On the Pods of the plugin, as its chart labels them.
PLUGIN = 'app.kubernetes.io/name=blockoverlayfs'
# On what a run creates in the cluster.
LABEL = 'app=blockoverlayfs-e2e'
ACCOUNT = 'blockoverlayfs-e2e'
# The suite's namespace, E2E_SUITE_NAMESPACE, is the value of this label on every namespace the suite creates:
# suiteNamespace and suiteLabel in test/e2e/suite_test.go.
SUITE_NAMESPACE = 'bofs-e2e'
SUITE_KEY = 'blockoverlayfs.io/e2e'
# What kubectl does in the suite: it creates namespaces with Pods and a Deployment in them, runs commands in those
# Pods, and reads the plugin's Pod, configuration and log, and the node.
RULES = [
    {'apiGroups': [''], 'resources': ['namespaces'], 'verbs': ['get', 'list', 'watch', 'create', 'patch', 'delete']},
    {'apiGroups': [''], 'resources': ['pods'], 'verbs': ['get', 'list', 'watch', 'create', 'delete']},
    {'apiGroups': [''], 'resources': ['pods/exec'], 'verbs': ['get', 'create']},
    {'apiGroups': [''], 'resources': ['pods/log', 'configmaps', 'nodes'], 'verbs': ['get']},
    {'apiGroups': [''], 'resources': ['events'], 'verbs': ['get', 'list']},
    {'apiGroups': ['apps'], 'resources': ['deployments'], 'verbs': ['get', 'list', 'watch', 'create']},
    {'apiGroups': ['apps'], 'resources': ['deployments/scale'], 'verbs': ['get', 'update', 'patch']},
    {'apiGroups': ['apps'], 'resources': ['replicasets'], 'verbs': ['get', 'list', 'watch']},
]
POD_MINUTES = 45
FAILED_STATES = ('failed', 'panicked', 'interrupted', 'aborted', 'timedout')
VERDICTS = {'passed': '通过', 'failed': '失败', 'error': '没有跑完'}
DECISIONS = {'allow': '放行', 'block': '阻断'}
RESULTS = {'passed': '通过', 'failed': '失败', 'skipped': '跳过', 'not_run': '未运行'}
START = time.monotonic()
BEGAN = datetime.datetime.now().astimezone()

NODES = list(dict.fromkeys(n for n in re.split(r'[,\s]+', os.environ.get('E2E_NODE', '')) if n))
SELECTOR = os.environ.get('E2E_NODE_SELECTOR', '').strip()
IMAGE = os.environ.get('E2E_IMAGE', '').strip()
# Where the cluster pulls the suite's image from.
TEST_IMAGE = os.environ.get('E2E_TEST_IMAGE', '').strip() or '/'.join(IMAGE.split('/')[:-1] + ['busybox:1.36'])
NS = os.environ.get('E2E_NAMESPACE', '').strip() or 'default'


def say(text):
    """Prints a line of progress, with the time since the start."""
    s = int(time.monotonic() - START)
    print('[%2dm%02ds] %s' % (s // 60, s % 60, text), flush=True)


def span(seconds):
    if seconds < 10:
        return '%.1fs' % seconds
    if seconds < 60:
        return '%.0fs' % seconds
    return '%dm%02ds' % (seconds // 60, seconds % 60)


def kubectl(*args, **kw):
    """Returns kubectl's output, and raises what it wrote to stderr if it failed."""
    timeout = kw.get('timeout', 30)
    p = subprocess.run(('kubectl', '--request-timeout=%ds' % timeout) + args, input=kw.get('stdin'),
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, universal_newlines=True, timeout=timeout + 10)
    if p.returncode:
        raise RuntimeError('kubectl %s：%s' % (' '.join(args[:4]), p.stderr.strip()[-600:]))
    return p.stdout


class Shard:
    """The part of a run that one node takes: one of the groups, or all of them, in a Pod of its own."""

    def __init__(self, run, groups, node, out):
        # The one shard of a run that is not dealt out has no groups, and the names of the run itself.
        self.groups, self.node, self.out = groups, node, out
        name = '-'.join(groups)
        self.tag = '[%s] ' % name if name else ''
        self.labels = ' || '.join(groups)
        self.pod = run + ('-' + name if name else '')
        self.suite_namespace = SUITE_NAMESPACE + ('-' + name if name else '')
        # The Pod as the cluster had it last, and its output so far.
        self.obj, self.lines, self.phase = {}, [], ''
        self.created = self.collected = self.over = False
        self.began, self.seconds, self.misses, self.done, self.told = 0.0, 0, 0, 0, 0.0
        # What the Pod says of how it ended, or what went wrong.
        self.result, self.error = None, ''


def ready(obj):
    """Whether a node or a Pod is ready."""
    return any(c['type'] == 'Ready' and c['status'] == 'True' for c in obj['status'].get('conditions') or [])


def candidates():
    """Returns the nodes the run may take, and whether the selector chose them. Each runs the cluster's plugin."""
    plugins = {p['spec'].get('nodeName') for p in
               json.loads(kubectl('get', 'pods', '--all-namespaces', '-l', PLUGIN, '-o', 'json'))['items'] if ready(p)}
    if NODES:
        for n in NODES:
            kubectl('get', 'node', n, '-o', 'name')
        without = [n for n in NODES if n not in plugins]
        if without:
            raise RuntimeError('这些节点上没有就绪的 blockoverlayfs，集群要先部署好它：' + '、'.join(without))
        return NODES, False
    if not SELECTOR:
        raise RuntimeError('E2E_NODE 和 E2E_NODE_SELECTOR 都没有给')
    fit = [n['metadata']['name'] for n in json.loads(kubectl('get', 'nodes', '-l', SELECTOR, '-o', 'json'))['items']
           if not n['spec'].get('unschedulable') and ready(n)]
    if not fit:
        raise RuntimeError('选择器 %s 没有选中 Ready 且可调度的节点' % SELECTOR)
    with_plugin = [n for n in fit if n in plugins]
    if not with_plugin:
        raise RuntimeError('选择器 %s 选中的 %d 个节点上都没有就绪的 blockoverlayfs，集群要先部署好它' % (SELECTOR, len(fit)))
    return with_plugin, True


def deal(count):
    """Deals the groups out to count nodes, longest first and back and forth, so that the nodes take about as long."""
    hands = [[] for _ in range(count)]
    for i, group in enumerate(LONGEST_FIRST):
        lap, at = divmod(i, count)
        hands[at if lap % 2 == 0 else count - 1 - at].append(group)
    return hands


def why(pod):
    """What the cluster says of a Pod that has not said how it ended."""
    status = pod.get('status', {})
    parts = [status.get('phase', ''), status.get('reason', ''), status.get('message', '')]
    for c in status.get('containerStatuses') or []:
        for state in (c.get('state') or {}).values():
            parts += [state.get('reason', ''), state.get('message', ''),
                      '退出码 %s' % state['exitCode'] if 'exitCode' in state else '']
    return '，'.join(p for p in parts if p)[:400]


def objects(shards):
    """What runs the suite: a Pod for each shard, and the access to the cluster that kubectl in them has."""
    labels = dict([LABEL.split('=')])
    pods = [{
        'apiVersion': 'v1', 'kind': 'Pod', 'metadata': {'name': s.pod, 'namespace': NS, 'labels': labels},
        'spec': {
            'restartPolicy': 'Never',
            'serviceAccountName': ACCOUNT,
            # Should this script not get to delete the Pod, the Pod ends by itself.
            'activeDeadlineSeconds': (POD_MINUTES + 5) * 60,
            'terminationGracePeriodSeconds': 10,
            # Not on a node under test, which has no Pod of the run but the suite's own. A field selector on nodes
            # takes one name for each requirement.
            'affinity': {'nodeAffinity': {'requiredDuringSchedulingIgnoredDuringExecution': {'nodeSelectorTerms': [
                {'matchFields': [{'key': 'metadata.name', 'operator': 'NotIn', 'values': [t.node]} for t in shards]}]}}},
            'containers': [{
                'name': 'suite', 'image': IMAGE,
                'env': [{'name': 'E2E_NODE', 'value': s.node},
                        {'name': 'E2E_TEST_IMAGE', 'value': TEST_IMAGE}, {'name': 'E2E_LABELS', 'value': s.labels},
                        {'name': 'E2E_SUITE_NAMESPACE', 'value': s.suite_namespace}],
                'volumeMounts': [{'name': 'work', 'mountPath': '/work'}],
            }],
            'volumes': [{'name': 'work', 'emptyDir': {}}],
        }} for s in shards]
    return {'apiVersion': 'v1', 'kind': 'List', 'items': [
        {'apiVersion': 'v1', 'kind': 'ServiceAccount', 'metadata': {'name': ACCOUNT, 'namespace': NS, 'labels': labels}},
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRole',
         'metadata': {'name': ACCOUNT, 'labels': labels}, 'rules': RULES},
        {'apiVersion': 'rbac.authorization.k8s.io/v1', 'kind': 'ClusterRoleBinding',
         'metadata': {'name': ACCOUNT, 'labels': labels},
         'roleRef': {'apiGroup': 'rbac.authorization.k8s.io', 'kind': 'ClusterRole', 'name': ACCOUNT},
         'subjects': [{'kind': 'ServiceAccount', 'name': ACCOUNT, 'namespace': NS}]},
    ] + pods}


def collect(name, out):
    """Fetches the Pod's /work/out. The Pod waits for that before it exits."""
    archive = out / 'pod-out.tgz'
    with archive.open('wb') as f:
        p = subprocess.run(('kubectl', '--request-timeout=120s', '-n', NS, 'exec', name, '--',
                            'tar', '-C', '/work/out', '-czf', '-', '.'),
                           stdout=f, stderr=subprocess.PIPE, timeout=150)
    if p.returncode:
        raise RuntimeError('取回报告失败：' + p.stderr.decode(errors='replace')[-600:])
    with tarfile.open(str(archive)) as tar:
        for m in tar.getmembers():
            base = os.path.basename(m.name)
            # Regular files of the directory itself, under names this script chooses.
            if m.isfile() and m.name in (base, './' + base):
                (out / base).write_bytes(tar.extractfile(m).read())
    archive.unlink()
    kubectl('-n', NS, 'exec', name, '--', 'touch', '/work/out/collected')


def follow(s):
    """Looks at a shard's Pod once: tells of its progress, fetches the suite's report once there is one, and reads
    how the Pod ended once it has."""
    try:
        s.obj = json.loads(kubectl('-n', NS, 'get', 'pod', s.pod, '-o', 'json'))
        s.misses = 0
    except (RuntimeError, subprocess.TimeoutExpired):
        # The API does not answer now and then, while the Pod goes on. A minute without an answer ends the shard.
        s.misses += 1
        if s.misses > 6:
            raise
        return
    phase = s.obj['status'].get('phase', '')
    if phase != s.phase:
        s.phase = phase
        say(s.tag + 'Pod 状态：' + phase)
    for c in s.obj['status'].get('containerStatuses') or []:
        waiting = (c.get('state') or {}).get('waiting') or {}
        if waiting.get('reason') in ('ErrImagePull', 'ImagePullBackOff', 'InvalidImageName'):
            # The kubelet goes on trying, to the Pod's deadline.
            raise RuntimeError('节点拉不到镜像 %s（%s）' % (IMAGE, waiting.get('message') or waiting['reason']))
    try:
        lines = kubectl('-n', NS, 'logs', s.pod).splitlines()
    except (RuntimeError, subprocess.TimeoutExpired):
        return
    for line in lines[len(s.lines):]:
        if line.startswith('## '):
            say(s.tag + line[3:])
    s.lines = lines
    # Ginkgo prints a line that starts with a bullet or an S for each spec it ends.
    finished = sum(1 for l in lines if l.startswith(('• [', 'S [')))
    if finished > s.done and time.monotonic() - s.told > 60:
        s.done, s.told = finished, time.monotonic()
        say('%s已跑完 %d 个用例' % (s.tag, s.done))
    if phase == 'Running' and not s.collected and any(l.startswith('E2E_RESULT=') for l in lines):
        collect(s.pod, s.out)
        s.collected = True
        say(s.tag + '已取回套件的报告')
    if phase not in ('Succeeded', 'Failed'):
        return
    (s.out / 'pod.log').write_text('\n'.join(lines) + '\n')
    marks = [l[len('E2E_RESULT='):] for l in lines if l.startswith('E2E_RESULT=')]
    if len(marks) != 1:
        raise RuntimeError('Pod 没有给出结果（%s）' % why(s.obj))
    if not s.collected:
        raise RuntimeError('Pod 结束前没能取回报告')
    s.result, s.over = json.loads(marks[0]), True


def run_suites(shards, owned):
    """Runs the suite in a Pod for each shard, and follows the Pods to their end. A shard that goes wrong ends there,
    and the others go on."""
    owned += [('serviceaccount', ACCOUNT), ('clusterrole', ACCOUNT), ('clusterrolebinding', ACCOUNT)]
    owned += [('pod', s.pod) for s in shards]
    kubectl('create', '-f', '-', stdin=json.dumps(objects(shards)))
    for s in shards:
        s.created, s.began = True, time.monotonic()
        say('%s已创建 Pod %s/%s' % (s.tag, NS, s.pod))

    deadline = time.monotonic() + POD_MINUTES * 60
    while not all(s.over for s in shards):
        time.sleep(10)
        for s in shards:
            if s.over:
                continue
            try:
                if time.monotonic() > deadline:
                    raise RuntimeError('Pod 在 %d 分钟内没有结束（%s）' % (POD_MINUTES, why(s.obj)))
                follow(s)
            except (RuntimeError, subprocess.TimeoutExpired, ValueError) as exc:
                s.error, s.over = str(exc), True
                say(s.tag + '出错：' + s.error)
            if s.over:
                s.seconds = int(time.monotonic() - s.began)


def cleanup(shards, owned):
    """Deletes what the run created, and what the suite left if it was cut short. Returns what could not be deleted."""
    errors = []
    for s in shards:
        if s.created and not (s.out / 'pod.log').exists():
            # The Pod has not said how it ended: keep what the cluster knows of it.
            for filename, args in (('events.txt', ('get', 'events', '--field-selector', 'involvedObject.name=' + s.pod)),
                                   ('describe.txt', ('describe', 'pod', s.pod)),
                                   ('pod.log', ('logs', s.pod))):
                try:
                    (s.out / filename).write_text(kubectl('-n', NS, *args))
                except Exception:
                    pass
    for kind, what in reversed(owned):
        try:
            kubectl('-n', NS, 'delete', kind, what, '--ignore-not-found', '--timeout=120s', timeout=150)
        except Exception as exc:
            errors.append(str(exc))
    if owned:
        try:
            kubectl('delete', 'namespaces', '-l', SUITE_KEY, '--timeout=300s', timeout=330)
        except Exception as exc:
            errors.append(str(exc))
    return errors


def read_report(path):
    """Reads Ginkgo's JSON report: one entry for each spec and each node of the suite's own, and what the suite
    recorded of its environment."""
    nodes, entries, succeeded = [], {}, True
    for suite in json.loads(path.read_text()):
        succeeded = succeeded and bool(suite.get('SuiteSucceeded'))
        for s in suite.get('SpecReports') or []:
            for e in s.get('ReportEntries') or []:
                entries[e.get('Name')] = (e.get('Value') or {}).get('Representation', '')
            labels = [l for ls in s.get('ContainerHierarchyLabels') or [] for l in ls or []] + (s.get('LeafNodeLabels') or [])
            failure = s.get('Failure') or {}
            where = failure.get('Location') or s.get('LeafNodeLocation') or {}
            state, message = s.get('State'), (failure.get('Message') or '').strip()
            if state == 'passed':
                result = 'passed'
            elif state in FAILED_STATES:
                result = 'failed'
            else:
                # A spec that skips itself says why. One that the label filter leaves out says nothing.
                result = 'skipped' if message else 'not_run'
            nodes.append({
                'kind': s.get('LeafNodeType'),
                'group': next((l for l in labels if l in GROUPS), ''),
                'name': ' '.join((s.get('ContainerHierarchyTexts') or []) + [s.get('LeafNodeText') or '']).strip()
                        or s.get('LeafNodeType'),
                'result': result,
                'state': state,
                'seconds': round((s.get('RunTime') or 0) / 1e9, 1),
                'location': '%s:%s' % (os.path.basename(where.get('FileName', '')), where.get('LineNumber', '')),
                'message': message,
                'output': s.get('CapturedGinkgoWriterOutput') or '',
            })
    return nodes, entries, succeeded


def tally(specs):
    counts = {r: sum(1 for s in specs if s['result'] == r) for r in RESULTS}
    counts['seconds'] = round(sum(s['seconds'] for s in specs), 1)
    return counts


def judge(pod, counts, failures, succeeded):
    """Returns the verdict on a shard, and why it is not passed. failures has the suite's own nodes besides its
    specs."""
    if failures or not succeeded:
        nodes = [f['kind'] for f in failures if f['kind'] != 'It']
        parts = ['%d 个用例失败' % counts['failed']] if counts['failed'] else []
        if nodes:
            # Ginkgo runs no spec once BeforeSuite has failed.
            parts.append('套件的 %s 环节失败%s' % ('、'.join(nodes), '' if counts['passed'] else '，用例没有运行'))
        return 'failed', '，'.join(parts) or 'Ginkgo 的报告里套件未成功'
    if pod['exit'] != 0:
        return 'error', '套件的退出码是 %s，而报告里没有失败的用例' % pod['exit']
    if not counts['passed']:
        return 'error', '没有用例运行'
    return 'passed', ''


def conclude(prefix, shards, selected, error, leftovers):
    """Reads the suite's reports and returns everything there is to say of the run. The files are written from it."""
    specs, failures, entries = {}, [], {}
    for s in shards:
        nodes, entries[s.node], succeeded, wrong = [], {}, False, s.error
        if not wrong and s.result:
            if not (s.out / 'report.json').exists():
                wrong = '套件以退出码 %s 结束，没有产出报告' % s.result['exit']
            else:
                nodes, entries[s.node], succeeded = read_report(s.out / 'report.json')
        for n in nodes:
            n['node'] = s.node
        mine = [n for n in nodes if n['kind'] == 'It']
        fails = [n for n in nodes if n['result'] == 'failed']
        failures += fails
        if error or wrong or not s.result:
            s.verdict, s.reason = 'error', wrong or error or 'Pod 没有给出结果'
        else:
            s.verdict, s.reason = judge(s.result, tally(mine), fails, succeeded)
        # Each shard's report has every spec: those of the other shards' groups as not run.
        for n in mine:
            key = (n['group'], n['name'])
            if key not in specs or specs[key]['result'] == 'not_run':
                specs[key] = n
    specs = list(specs.values())
    counts = dict(total=len(specs), **tally(specs))

    if error or not shards:
        verdict, reason = 'error', error
    else:
        verdict = next(v for v in ('failed', 'error', 'passed') if any(s.verdict == v for s in shards))
        reason = '；'.join(s.tag + s.reason for s in shards if s.verdict != 'passed')
    decision = 'allow' if verdict == 'passed' else 'block'

    tail = []
    for s in shards:
        if s.verdict == 'error' and (s.out / 'pod.log').exists():
            tail += [s.tag + l[:200] for l in (s.out / 'pod.log').read_text(errors='replace').splitlines()[-25:]]
    tools = next((s.result for s in shards if s.result), {})

    def told(entry):
        """What the suites recorded under a name, without repeats."""
        return '，'.join(dict.fromkeys(v for v in (entries[s.node].get(entry, '') for s in shards) if v))

    return {
        'decision': decision, 'verdict': verdict, 'reason': reason,
        'started_at': BEGAN.isoformat(timespec='seconds'), 'seconds': int(time.monotonic() - START),
        'suite_image': IMAGE,
        # As the nodes that ran the suite's Pods have it.
        'suite_image_id': '，'.join(dict.fromkeys(
            c['imageID'] for s in shards for c in s.obj.get('status', {}).get('containerStatuses') or [] if c.get('imageID'))),
        'plugin': told('plugin'), 'plugin_image': told('plugin image'), 'running_image': told('plugin image id'),
        'nodes_from': 'selector' if selected else 'list',
        'shards': [{'groups': s.groups, 'labels': s.labels, 'node': s.node, 'node_info': entries[s.node].get('node', ''),
                    'plugin': entries[s.node].get('plugin', ''), 'running_image': entries[s.node].get('plugin image id', ''),
                    'pod': s.pod if s.created else '', 'suite_namespace': s.suite_namespace, 'seconds': s.seconds,
                    'verdict': s.verdict, 'reason': s.reason} for s in shards],
        'tools': {k: tools[k] for k in ('kubectl',) if k in tools},
        'namespace': NS,
        'counts': counts,
        'groups': {g: tally([s for s in specs if s['group'] == g]) for g in GROUPS},
        'failures': [{k: n[k] for k in ('kind', 'group', 'name', 'node', 'state', 'location', 'message', 'output')}
                     for n in failures],
        'specs': [{k: s[k] for k in ('group', 'name', 'node', 'result', 'seconds', 'location', 'message')} for s in specs],
        'not_cleaned': leftovers,
        'log_tail': tail,
        'files': {k: prefix.name + suffix for k, suffix in (('report', '.md'), ('result', '.json'), ('log', '.log'))},
    }


def block(text, limit, rest):
    """A block of text as it is, cut at limit characters with a note of where the rest is."""
    if len(text) > limit:
        text = text[:limit] + '\n…（截断，全文见 %s）' % rest
    return ['````'] + text.splitlines() + ['````', '']


def render(doc, full):
    """Returns the report in Markdown, or its summary: that cuts the failures short and leaves out what the suite
    wrote when a spec failed, and the list of specs."""
    files, shards = doc['files'], doc['shards']
    many = len(shards) > 1
    lines = ['# blockoverlayfs e2e：%s' % VERDICTS[doc['verdict']], '',
             '**结论：%s**%s' % (DECISIONS[doc['decision']], '——' + doc['reason'] if doc['reason'] else ''), '']

    def ran_on(group):
        return '、'.join(sorted({s['node'] for s in doc['specs'] if s['group'] == group and s['result'] != 'not_run'}))

    if doc['specs']:
        head = ['分组'] + (['节点'] if many else []) + ['通过', '失败', '跳过', '未运行', '用时']
        lines += ['| %s |' % ' | '.join(head), '|%s' % ('---|' * len(head))]
        for group, c in [(g, doc['groups'][g]) for g in GROUPS] + [('合计', doc['counts'])]:
            row = [group] + ([ran_on(group) or '–'] if many else [])
            row += ['%d' % c[r] for r in RESULTS] + [span(c['seconds']) if c['seconds'] else '–']
            lines.append('| %s |' % ' | '.join(row))
        lines.append('')

    def fact(name, value):
        return ['- %s：`%s`' % (name, value)] if value else []

    picked = '随机选中' if doc['nodes_from'] == 'selector' else '指定'
    lines += fact('套件的镜像', doc['suite_image']) + fact('套件镜像的摘要', doc['suite_image_id'])
    lines += fact('集群里的插件', doc['plugin']) + fact('插件的镜像', doc['plugin_image']) + fact('插件镜像的摘要', doc['running_image'])
    if many:
        lines.append('- 测试节点：%d 个同时跑，%s' % (len(shards), picked))
        for s in shards:
            where = 'Pod `%s/%s`，用时 %s' % (doc['namespace'], s['pod'], span(s['seconds'])) if s['pod'] else '没有建 Pod'
            lines.append('  - %s：`%s`，%s%s' % ('、'.join(s['groups']), s['node_info'] or s['node'], where,
                                              '' if s['verdict'] == 'passed' else '，' + VERDICTS[s['verdict']]))
    elif shards:
        lines += fact('测试节点', '%s（%s）' % (shards[0]['node_info'] or shards[0]['node'], picked))
    lines += fact('工具', '，'.join('%s %s' % (k, v) for k, v in doc['tools'].items()))
    if not many and shards and shards[0]['pod']:
        lines += fact('运行套件的 Pod', '%s/%s' % (doc['namespace'], shards[0]['pod']))
    lines += fact('开始时间', '%s，共 %s' % (doc['started_at'].replace('T', ' '), span(doc['seconds'])))
    lines += fact('没有清理掉', '；'.join(doc['not_cleaned'])) + ['']

    if doc['failures']:
        lines += ['## 失败的用例和环节', '']
        for i, f in enumerate(doc['failures'], 1):
            lines += ['### %d. [%s] %s' % (i, f['group'] or f['kind'], f['name']), '',
                      '位置 `%s`，状态 `%s`，节点 `%s`。' % (f['location'], f['state'], f['node']), '']
            lines += block(f['message'], 6000 if full else 600, files['result'] if full else files['report'])
            if full and f['output']:
                lines += ['失败时插件和节点的状态：', ''] + block(f['output'], 30000, files['result'])
        if not full:
            lines += ['失败时插件和节点的状态在 `%s` 里。' % files['report'], '']
    skipped = [s for s in doc['specs'] if s['result'] == 'skipped']
    if skipped:
        lines += ['## 被跳过的用例', '', '环境不具备这些用例要的条件，它们不影响结论。', '']
        lines += ['- [%s] %s——%s' % (s['group'], s['name'], s['message'].splitlines()[0]) for s in skipped] + ['']
    if doc['log_tail']:
        lines += ['## Pod 日志的最后几行', ''] + block('\n'.join(doc['log_tail']), 6000, files['log'])
    if full and doc['specs']:
        lines += ['## 全部用例', '']
        for group in GROUPS:
            specs = [s for s in doc['specs'] if s['group'] == group]
            if specs:
                lines += ['### ' + group + ('，节点 `%s`' % ran_on(group) if many and ran_on(group) else ''), '',
                          '| 用例 | 结果 | 用时 |', '|---|---|---|']
                lines += ['| %s | %s | %s |' % (s['name'].replace('|', '\\|'), RESULTS[s['result']],
                                               span(s['seconds']) if s['result'] in ('passed', 'failed') else '–')
                          for s in specs] + ['']
    if full:
        lines += ['## 同一次运行的其他文件', '',
                  '- `%s`：这份报告的结构化版本，另有每个失败用例的完整现场。' % files['result'],
                  '- `%s`：运行套件的 Pod 的完整输出。' % files['log'], '']
    text = '\n'.join(lines)
    if not full and len(text) > 6000:
        text = text[:6000] + '\n\n…（截断，全文见 %s）' % files['report']
    return text


def write(prefix, doc, shards):
    """Writes the report, the result and the log."""
    prefix.parent.mkdir(parents=True, exist_ok=True)
    pathlib.Path(str(prefix) + '.md').write_text(render(doc, True) + '\n')
    pathlib.Path(str(prefix) + '.json').write_text(json.dumps(doc, ensure_ascii=False, indent=2) + '\n')
    logs = []
    for s in shards:
        for title, filename in (('Pod %s/%s 的输出' % (NS, s.pod), 'pod.log'),
                                ('kubectl describe pod %s' % s.pod, 'describe.txt'), ('Pod %s 的事件' % s.pod, 'events.txt')):
            if (s.out / filename).exists():
                logs.append('==== %s%s ====\n%s\n' % (s.tag, title, (s.out / filename).read_text(errors='replace').rstrip('\n')))
    pathlib.Path(str(prefix) + '.log').write_text('\n'.join(logs) or '运行套件的 Pod 没有建出来，所以没有日志。\n')


def run(prefix):
    name = 'blockoverlayfs-e2e-' + BEGAN.strftime('%Y%m%d-%H%M%S')
    out = pathlib.Path(tempfile.mkdtemp())
    owned, shards, error, selected = [], [], '', True
    try:
        if not IMAGE:
            raise RuntimeError('E2E_IMAGE 没有给')
        kubectl('get', 'namespace', NS, '-o', 'name')
        busy = ['%s/%s' % (p['metadata']['namespace'], p['metadata']['name'])
                for p in json.loads(kubectl('get', 'pods', '--all-namespaces', '-l', LABEL, '-o', 'json'))['items']
                if p['status'].get('phase') in ('Pending', 'Running')]
        if busy:
            # Two runs would use the same namespaces.
            raise RuntimeError('已有一次运行在进行：' + ' '.join(busy))
        # What a run that was killed left behind.
        kubectl('-n', NS, 'delete', 'pods,serviceaccounts', '-l', LABEL, '--ignore-not-found', timeout=150)
        kubectl('delete', 'clusterrolebindings,clusterroles', '-l', LABEL, '--ignore-not-found')
        kubectl('delete', 'namespaces', '-l', SUITE_KEY, '--timeout=300s', timeout=330)

        nodes, selected = candidates()
        # A node for each group. With one node to run on, one Pod runs every group there.
        count = min(len(GROUPS), len(nodes))
        for groups, node in zip(deal(count) if count > 1 else [[]], random.sample(nodes, count)):
            shards.append(Shard(name, groups, node, out / node))
            shards[-1].out.mkdir()
            say('%s测试节点：%s（%s）' % (shards[-1].tag, node, '随机选中' if selected else '指定'))
        run_suites(shards, owned)
    except (RuntimeError, subprocess.TimeoutExpired, ValueError) as exc:
        error = str(exc)
        say('出错：' + error)
    finally:
        # Also when the run is cancelled.
        leftovers = cleanup(shards, owned)

    doc = conclude(prefix, shards, selected, error, leftovers)
    write(prefix, doc, shards)
    shutil.rmtree(str(out))
    counts = doc['counts']
    say('结论：%s（%s）——通过 %d，失败 %d，跳过 %d，未运行 %d' % (
        DECISIONS[doc['decision']], VERDICTS[doc['verdict']],
        counts['passed'], counts['failed'], counts['skipped'], counts['not_run']))
    for f in doc['failures']:
        say('失败：[%s] %s（%s，节点 %s）' % (f['group'] or f['kind'], f['name'], f['location'], f['node']))
    return 0 if doc['decision'] == 'allow' else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    commands = parser.add_subparsers(dest='command')
    commands.required = True
    commands.add_parser('run').add_argument('--out', required=True, metavar='PREFIX')
    again = commands.add_parser('render')
    again.add_argument('--summary', action='store_true')
    again.add_argument('result', metavar='PREFIX.json')
    args = parser.parse_args()
    if args.command == 'render':
        print(render(json.loads(pathlib.Path(args.result).read_text()), not args.summary))
        return 0
    # A run that is cancelled is told to end: as an exception, that takes the way through run's cleanup.
    for sig in (signal.SIGTERM, signal.SIGHUP):
        signal.signal(sig, lambda *a: sys.exit(143))
    return run(pathlib.Path(args.out))


if __name__ == '__main__':
    sys.exit(main())
