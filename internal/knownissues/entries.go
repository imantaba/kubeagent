package knownissues

// entries is the curated content: one entry per failure kind
// diagnose.DefaultDetectors can emit, sorted by Kind so All and Kinds need no
// sort at call time.
//
// This is a Go slice literal rather than an embedded data file on purpose. A
// data file would need a parser, an error path for a malformed entry, and a
// dependency decision the no-new-dependency rule forecloses — and it would buy
// nothing, because a contributor edits prose inside a struct either way. Here,
// `go build` rejects a malformed entry and `go vet` sees the whole thing.
var entries = []Entry{
	{
		Kind:    "ContainerStartError",
		Summary: "the container was created but could not be started",
		Detail: "The image was resolved and the container object was created, and then the " +
			"runtime failed to start the process inside it. This is the kind kubeagent " +
			"reports when it can see that a container did not start and cannot say why: " +
			"the kubelet's own reason and message are quoted verbatim in the evidence and " +
			"are the whole diagnosis. There are no logs, because the process never ran. " +
			"It is reported at medium confidence for that reason — a start failure is a " +
			"fact, its cause is not.",
		Causes: []string{
			"The container was killed by the OOM killer during startup — the memory limit is below what the process needs merely to start.",
			"A postStart or preStop lifecycle hook exited non-zero, which fails the container even though the process itself was fine.",
			"The command or entrypoint names a path that does not exist in the image, or a file that is not executable.",
			"A read-only root filesystem, a securityContext the runtime refuses, or a device the node does not have.",
			"The container runtime itself is unhealthy on that node — a full disk, an exhausted PID limit, a broken snapshotter.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the waiting reason and message the kubelet recorded, and the last terminated state",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.containerStatuses[*].lastState}' — a StartError here often names an OOM kill the pod's own reason does not",
			"The container's memory limit against what the process needs to start, not what it needs at rest",
			"kubectl describe node <node> — whether other pods are failing to start on the same node, which points at the runtime rather than the workload",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#containerstarterror",
	},
	{
		Kind:    "CrashLoopBackOff",
		Summary: "a container starts, exits, and is restarted on a widening backoff",
		Detail: "Kubernetes started the container, the process exited, and the kubelet " +
			"restarted it — over and over. After the first few attempts the kubelet waits " +
			"longer between them, which is the BackOff in the name. The container is not " +
			"stuck: it is running briefly and failing every time.",
		Causes: []string{
			"The process exits immediately — a missing environment variable, an unreachable dependency, or a bad command.",
			"The image's entrypoint is wrong, so the container runs and exits straight away.",
			"A liveness probe kills the container before it finishes starting; a startup probe is the fix, not a longer liveness period.",
			"The application panics on a configuration value it cannot parse.",
			"A file the process needs at startup is absent from the mount it expects.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the crashed run, not the current one",
			"kubectl -n <namespace> describe pod <pod> — the exit code and the last terminated state",
			"The container's command and args against what the image expects to be told to run",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#crashloopbackoff",
	},
	{
		Kind:    "CreateContainerConfigError",
		Summary: "the kubelet cannot build the container from its spec",
		Detail: "The kubelet accepted the pod, tried to assemble the container's " +
			"configuration, and could not: something the spec references does not exist. " +
			"The container never reaches Running, so there are no logs to read — the " +
			"kubelet's waiting message names the missing object and is the whole diagnosis. " +
			"Unlike an event, that state persists for as long as the container is stuck.",
		Causes: []string{
			"The ConfigMap or Secret has not been created yet — manifests applied out of order.",
			"It exists in another namespace; a pod can only reference objects in its own.",
			"The object exists but the key named in configMapKeyRef or secretKeyRef does not.",
			"A typo in the object name in the pod spec.",
			"A controller that generates the object has not reconciled yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the waiting message names the missing object",
			"kubectl -n <namespace> get configmap,secret — whether the referenced object is there at all",
			"kubectl -n <namespace> get configmap <name> -o jsonpath='{.data}' — whether it holds the keys the pod asks for",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#createcontainerconfigerror",
	},
	{
		Kind:    "ErrImagePull",
		Summary: "the kubelet's attempt to pull the image failed",
		Detail: "The kubelet asked the container runtime to pull the image and the pull " +
			"returned an error: the tag does not exist, the registry wants credentials the " +
			"node does not have, or the registry could not be reached. This is the first " +
			"failure; after several the kubelet backs off and the state becomes " +
			"ImagePullBackOff. Same problem, one stage later.",
		Causes: []string{
			"The tag does not exist in the registry — a typo, or a tag that was never pushed.",
			"The registry is private and the pod has no imagePullSecret, or the secret lives in another namespace.",
			"The node cannot reach the registry — egress firewall, proxy, or DNS.",
			"The image reference omits a registry and defaults to the public one, which is not where the image lives.",
			"An anonymous-pull rate limit is being applied to the node.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Failed event carries the registry's own error",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].image}' — the exact reference being pulled",
			"kubectl -n <namespace> get serviceaccount <name> -o yaml — whether an imagePullSecret is attached",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#imagepullbackoff-errimagepull",
	},
	{
		Kind:    "ImagePullBackOff",
		Summary: "repeated pull failures, now backing off between attempts",
		Detail: "The same failure ErrImagePull describes, after the kubelet stopped " +
			"retrying immediately. The pod can sit here for minutes with nothing new in its " +
			"events, which makes it look like a hang rather than a pull error. Correcting " +
			"the image or the credential does not always clear it at once: the kubelet " +
			"retries on its own schedule.",
		Causes: []string{
			"Every ErrImagePull cause — this is that failure once the kubelet began backing off.",
			"The image or the credential was corrected, but the kubelet has not retried yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the pull error, unchanged from the first attempt",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].image}' — the exact reference being pulled",
			"kubectl -n <namespace> get events --field-selector involvedObject.name=<pod> — how far apart the attempts now are",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#imagepullbackoff-errimagepull",
	},
	{
		Kind:    "Init:CrashLoopBackOff",
		Summary: "an init container is crash-looping, so the pod never starts",
		Detail: "Init containers run one at a time, in order, and each must succeed before " +
			"the next begins. One is crashing and being restarted, which blocks the pod " +
			"indefinitely: the main containers have not run at all, so their logs are empty " +
			"and their status says only PodInitializing. The failure — and the logs — are in " +
			"the init container.",
		Causes: []string{
			"A wait-for-dependency init container cannot reach the service or endpoint it polls.",
			"The init container's own command is wrong, or its script exits non-zero.",
			"A migration or setup step fails against a backend that is not ready.",
			"A volume the init container writes to is read-only, or not mounted where it expects.",
			"The init step has no timeout, so a permanent failure looks like a slow start.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the failing init container, not a main one",
			"kubectl -n <namespace> describe pod <pod> — which init container, its position, and its restart count",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.initContainerStatuses}' — the whole init phase at once",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:CreateContainerConfigError",
		Summary: "the kubelet cannot build an init container from its spec",
		Detail: "The configuration failure CreateContainerConfigError describes, on an init " +
			"container instead of a main one. The consequence differs: the init sequence " +
			"stops at that container, so nothing later runs and the main containers are " +
			"never created — their status says only PodInitializing, which names neither " +
			"the missing object nor the container that wanted it. The kubelet's waiting " +
			"message on the init container is the whole diagnosis, and it persists for as " +
			"long as the container is stuck.",
		Causes: []string{
			"Every CreateContainerConfigError cause — this is that failure on an init container.",
			"An init step often reads a different Secret from the workload: a migration credential, a bootstrap token.",
			"The ConfigMap or Secret is created by a main-container sidecar, or by a Job that has not run yet, so nothing exists when the init phase starts.",
			"A key was renamed in the object and only the init container's secretKeyRef still names the old one.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the waiting message names the init container and the missing object",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.initContainerStatuses}' — which init container, and its position in the sequence",
			"kubectl -n <namespace> get configmap,secret — whether the referenced object exists in this namespace at all",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:ErrImagePull",
		Summary: "an init container's image could not be pulled",
		Detail: "The pull failure is the one ErrImagePull describes, on an init container " +
			"instead of a main one. The consequence differs: init containers block the pod, " +
			"so nothing later runs and the main containers' images are never even fetched. " +
			"An init step often uses a different image from the workload — a small utility, " +
			"a migration tool — and it is easy for that one to miss a credential the main " +
			"image has.",
		Causes: []string{
			"The init image's tag does not exist, or the reference has a typo.",
			"The init image lives in a registry the pod has no imagePullSecret for, even though the main image does not.",
			"The node cannot reach that registry — egress firewall, proxy, or DNS.",
			"A utility image was pinned to a tag that has since been deleted.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Failed event names the init container and the pull error",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].image}' — the exact init references",
			"kubectl -n <namespace> get serviceaccount <name> -o yaml — whether an imagePullSecret is attached",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:ImagePullBackOff",
		Summary: "an init container's image pull is backing off",
		Detail: "Init:ErrImagePull after the kubelet started waiting between attempts. The " +
			"pod is wedged at that init container and will stay there: nothing later in the " +
			"sequence runs, and the main containers are never started. The pod's age climbs " +
			"while the events go quiet, which is what makes this read as a hang.",
		Causes: []string{
			"Every Init:ErrImagePull cause — this is that failure once the kubelet began backing off.",
			"The init image or its credential was corrected, but the kubelet has not retried yet.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the pull error, unchanged from the first attempt",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].image}' — the exact init references",
			"kubectl -n <namespace> get events --field-selector involvedObject.name=<pod> — how far apart the attempts now are",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "Init:OOMKilled",
		Summary: "an init container was killed for exceeding its memory limit",
		Detail: "The kernel's OOM killer terminated an init container, and the pod is " +
			"blocked in its init phase. Init steps are often given small limits, or none of " +
			"their own, on the assumption that they are cheap — a migration, a restore or an " +
			"archive extraction breaks that assumption. The exit code is 137. Raising the " +
			"limit is not automatically right: a step that grows without bound will exceed " +
			"any limit.",
		Causes: []string{
			"The limit is smaller than the work the step actually does.",
			"It loads a whole file or dataset into memory instead of streaming it.",
			"It inherits a low default from a LimitRange it was never sized against.",
			"The work grew: the same step passed when the dataset was smaller.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the init container's exit code and last terminated state",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.initContainers[*].resources}' — the limits it runs under",
			"kubectl -n <namespace> get limitrange -o yaml — a namespace default that may be capping it",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#init-container-failures",
	},
	{
		Kind:    "OOMKilled",
		Summary: "the kernel killed a container for exceeding its memory limit",
		Detail:  "The kernel killed a container for exceeding its memory limit.",
		Causes: []string{
			"The limit is lower than the workload's real steady-state usage.",
			"A leak: usage climbs until the limit is reached, then repeats on a cycle.",
			"A runtime heap sized above the container limit, so the runtime never reclaims before the kernel intervenes.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — lastState.terminated.exitCode 137",
			"kubectl -n <namespace> top pod <pod> — usage against the configured limit",
			"The container's own memory tuning against resources.limits.memory",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#oomkilled",
	},
	{
		Kind:    "ProbeFailure",
		Summary: "a container is running but a probe keeps failing",
		Detail: "The container is Running and the pod is not Ready, because a kubelet probe " +
			"against it keeps failing. Which probe decides what happens next: a readiness " +
			"failure takes the pod out of its Service's endpoints and leaves it running, a " +
			"liveness failure makes the kubelet restart the container, and a startup failure " +
			"means the container is restarted before it ever finishes starting. kubeagent " +
			"names the probe and a coarse reason, and never carries the raw probe message, " +
			"which can hold a pod address or arbitrary exec output.",
		Causes: []string{
			"initialDelaySeconds is shorter than the application's real startup time; a startup probe is the fix, not a longer liveness period.",
			"The probe targets the wrong port or the wrong path.",
			"The application is genuinely unhealthy and answering with an error status.",
			"A dependency the health endpoint checks is down, so the probe reports the dependency's failure as the pod's.",
			"timeoutSeconds is shorter than the endpoint's response time under load.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the Unhealthy events and the probe definitions",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.containers[*].readinessProbe}' — the port, path and timings",
			"kubectl -n <namespace> logs <pod> -c <container> — whether the application logs the failing requests",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#probefailure",
	},
	{
		Kind:    "RestartLoop",
		Summary: "a container keeps exiting and restarting while still Running",
		Detail: "A flapping container: several restarts, the last one recent, the previous " +
			"run ended non-zero and not by an OOM kill — and the container happens to be " +
			"Running at the moment kubeagent looked. This is the case CrashLoopBackOff " +
			"misses, because that condition is only visible while the kubelet is waiting " +
			"between attempts. A pod that restarts every few minutes can look healthy in " +
			"every point-in-time check and never stay up.",
		Causes: []string{
			"An intermittent failure — a dependency that is up most of the time, or a request that occasionally kills the process.",
			"A liveness probe that fails under load and restarts a container that would have recovered.",
			"The process treats a recurring condition, such as a dropped connection, as fatal.",
			"A limit other than memory — ephemeral storage, for instance — terminating the container.",
			"A controller or operator restarting the workload on a schedule.",
		},
		Checks: []string{
			"kubectl -n <namespace> logs <pod> -c <container> --previous — the run that exited is the one that failed",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.status.containerStatuses[*].restartCount}' — how fast it is climbing",
			"kubectl -n <namespace> describe pod <pod> — the last terminated state's exit code and reason",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#restartloop",
	},
	{
		Kind:    "Unschedulable",
		Summary: "no node can place the pod",
		Detail: "The API server accepted the pod and the scheduler cannot find a node that " +
			"satisfies it, so it stays Pending with nothing running. The scheduler records " +
			"why on the pod's PodScheduled condition, usually with a count of how many nodes " +
			"were rejected and for which reason — insufficient capacity and an unsatisfied " +
			"taint read very differently there.",
		Causes: []string{
			"No node has enough allocatable CPU or memory left for the pod's requests.",
			"Every candidate node carries a taint the pod does not tolerate.",
			"A nodeSelector, node affinity or topology constraint no node matches.",
			"The pod needs a volume that can only attach in a zone with no room left.",
			"The cluster has no nodes in a Ready state at all.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the scheduler's own message on the PodScheduled condition",
			"kubectl get nodes -o wide — whether nodes are Ready, and how many there are",
			"kubectl describe node <node> — its taints and its allocated-resources summary",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#pending-unschedulable",
	},
	{
		Kind:    "VolumeAttachError",
		Summary: "a volume cannot be attached, so the container never starts",
		Detail: "The pod is scheduled and the kubelet cannot start it: a volume it needs is " +
			"not attached to the node it landed on. The common shape is Multi-Attach — a " +
			"ReadWriteOnce volume is still attached to the node the previous pod ran on, and " +
			"it cannot be attached in two places at once. That clears itself once the old " +
			"attachment is released, which can take several minutes after a node failure; " +
			"when it does not clear, the old node is usually gone without ever detaching.",
		Causes: []string{
			"A ReadWriteOnce volume is still attached to the node the previous pod ran on.",
			"The node that held the volume was lost, so the detach never completed.",
			"The CSI driver on the target node is not running, or not healthy.",
			"The volume is in a different zone from the node the pod was scheduled to.",
			"The storage backend refused the attach — a quota, a per-node device limit, or an unavailable volume.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the FailedAttachVolume events and what they name",
			"kubectl get volumeattachment — which node the volume is currently attached to",
			"kubectl -n <namespace> get pvc — the claim's phase and the volume it is bound to",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#volumeattacherror",
	},
	{
		Kind:    "VolumeMountError",
		Summary: "a volume cannot be mounted, so the container never starts",
		Detail: "The pod is scheduled, its volumes were accepted by the API server, and the " +
			"kubelet cannot set one of them up. Despite the name this is most often not a " +
			"storage problem at all: a ConfigMap or Secret named in the pod spec as a volume " +
			"source does not exist. That case produces no CreateContainerConfigError — the " +
			"kubelet reports a container config error only for env and envFrom references — " +
			"so the container sits in ContainerCreating and the kubelet's FailedMount event " +
			"is the only place the missing object is named. Events expire after about an " +
			"hour, and the pod stays stuck long after that.",
		Causes: []string{
			"A ConfigMap or Secret used as a volume source does not exist, or is in another namespace.",
			"The object name in the volume's configMap/secret block has a typo the env path would have caught elsewhere.",
			"A projected volume names a key that is not in the object.",
			"An NFS or CIFS server is unreachable, or the export path does not exist.",
			"The CSI driver on the node cannot stage or publish the volume, so the mount times out.",
			"A subPath refers to a directory that the volume does not contain.",
		},
		Checks: []string{
			"kubectl -n <namespace> describe pod <pod> — the FailedMount events, which name the volume and the missing object",
			"kubectl -n <namespace> get configmap,secret — whether the referenced source exists in this namespace",
			"kubectl -n <namespace> get pod <pod> -o jsonpath='{.spec.volumes}' — every volume source the pod asks for",
		},
		Docs: "https://k8sproject.top/features/diagnostics/#volumemounterror",
	},
}
