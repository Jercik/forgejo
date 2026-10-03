# Actions maintenance contract

This fork supports routine maintenance with unchanged Forgejo runner v13.2.0.
The first enabling rollout needs a separately proved cutover. A new capability
or migration cannot establish that older tasks reported successfully.

Set `[actions] ADMISSION_LOCK_PATH` to an absolute readable regular file. Keep
the file and its parent directory protected from replacement by untrusted
users. Mount the file into Forgejo, rather than mounting a directory where the
file can be replaced. Configured startup rejects a missing or unsafe file.
Assignment errors on changed paths deny new work. Unsupported operating systems
reject the configured feature; leaving it unset disables the admission API.

Each assignment holds an independent shared `flock` through its transaction and
task construction. A host exclusive owner waits for existing shared holders,
then blocks new assignments. Existing request-key recovery and final reporting
remain available. Keep the same exclusive owner and file inode through every
protected stop, backup, restart and health check. Forgejo cannot establish that
host ownership survives a reboot; the cluster must retain its maintenance
marker and refuse ordinary startup after an interrupted operation.

The `actions-admission-drain` version capability means the configured contract
below is available. The existing authenticated admin endpoint
`GET /api/v1/admin/actions/runners/{id}` adds `admission`:

- `process_id`, `fetch_sequence`: server process UUID and global fetch-start
  sequence at the snapshot.
- `fenced`, `lock_device`, `lock_inode`: current exclusive-fence probe and file
  identity. A changed or missing file fails the API request.
- `pending_final_reports`, `uncovered_nonterminal_tasks`, `nonterminal_tasks`:
  global task counts. These do not join current runner records or omit tasks
  belonging to deleted runners.
- `last_fetch`: the latest completed successful bulk fetch, with request-start
  `sequence`, explicitly supplied nullable `task_capacity`, total `task_count`
  including recovery, and `fenced` at completion. An absent observation is NULL.

After exclusive acquisition, capture the process ID and baseline sequence. For
every live daemon require a later fetch-start sequence, the same process ID,
the daemon's proven live capacity, zero total returned tasks and a held fence.
Missing capacity cannot prove idle. A bulk response containing a task assigned
before fence acquisition cannot prove idle. Restarting Forgejo invalidates the
old process observations; obtain a fresh baseline and observations before the
next protected stop.

This proof requires a complete fleet census with exactly one supported daemon
per runner identity and the expected connection, executable, runtime config
and capacity. Forgejo's self-reported runner version and apparent idle status
do not establish those conditions. The cluster must reject unknown, duplicate
or changed clients and compare host/container file identity continuously.

The nullable `action_task.runner_final_report_received` field survives server
restart. NULL means legacy or server-only work; false means a tracked assignment
awaiting final-report acceptance; true records accepted runner reporting. New
assignments insert false in their assignment transaction. Recovery enrolls a
legacy runner task before delivery and never resets true.

An authenticated terminal `UpdateTask` records true only at successful handler
completion with every requested output key acknowledged. Ignored oversized
outputs leave the receipt pending. A retry can complete outputs after the task
status is already terminal. Runner v13.2.0 sends that terminal report only after
its final log upload succeeds. Server cancellation, zombie cleanup, terminal
status and log archival do not establish this receipt.

Require zero pending receipts, zero assigned nonterminal tasks and zero
uncovered nonterminal tasks, in addition to fresh local idle observations.
Idle means the runner's job and reporter returned; it does not mean reporting
succeeded. A pending receipt after exhausted runner retries blocks maintenance
and requires explicit reconciliation. Do not resolve it from elapsed time or
terminal status. Legacy terminal NULL rows can be excluded only after the
separate first-cutover guarantee establishes the coverage boundary.

Task and repository deletion refuse pending receipts under the same database
repository lock used by assignment and recovery. Scheduled expiration rereads
receipts under that lock and preserves pending logs. Accepted tasks retain
ordinary deletion and retention
behavior. The migration does not mark historical rows accepted. Returning to
an image without this migration requires the cluster's paired database and
data-volume restore procedure, rather than changing the image alone.
