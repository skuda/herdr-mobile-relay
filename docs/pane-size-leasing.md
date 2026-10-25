# Pane-size leasing: opt-in Resize Session

Resize Session lets a controller phone resize the shared pane to fit itself
while it has the pane open. It is a per-device opt-in, off by default, and it
is honest about its cost: **while a phone holds a lease, the desktop and SSH
views of that pane look garbled.** This page records the design, the safety
boundary every write passes through, the cleanup budget, and the risks that
remain. User-facing behavior is in
[mobile-app.md](mobile-app.md#mobile-terminal).

## Why the garbling is inherent

Herdr owns a pane's terminal grid and its PTY together. Herdr 0.9.3 (socket
protocol 22) has no operation that sets a pane's absolute rows and columns:
`pane.resize` only changes split ratios. The relay therefore sets the PTY's
window size directly (`TIOCSWINSZ`). The application redraws at the leased
size, Herdr's grid stays at the layout size, and every other client of that
pane sees the mismatch until the lease ends. No identity or lifecycle care on
the relay side can fix that; a Herdr-owned resize API is the proper fix (see
[Follow-up work](#follow-up-work)).

## Protocol and compatibility

| Element | Behavior |
| --- | --- |
| Capabilities | `pane_size_opt_in` and `pane_size_opt_in_rows`, advertised only by relays built for darwin or linux. `pane_size_lease` / `pane_size_lease_rows` are never advertised: apps up to 0.22.7 lease automatically when they see them. No operator flag. |
| `lease_pane_size` | Acquire and renew. Must carry `opt_in: true`; otherwise it fails with "Resize Session is unavailable on this relay" before any Herdr, process or TTY access. Carries `columns` (40–240) and optionally `rows` (10–120). Result data: applied `columns` and `rows`. |
| `release_pane_size` | Idempotent cleanup. Without `immediate` the lease lapses after a 10 s navigation grace; `immediate: true` (sent when the user turns the switch off) skips it. |
| Authorization | Both are controller-only ordered mutations with exact target validation (`validateExactPaneTarget`); release keeps the stale-target cleanup allowance. Readers are denied. |
| Disconnect | A departing connection's leases are released immediately. |
| Pane reads | The relay strips client-supplied `terminal_columns`/`terminal_rows`. A leasing client's response carries viewport geometry. Eligible no-scrollback Claude/Qoder panes use the pane-wide physical source described below, including reads from non-leasing clients. |
| Settling | History admission pauses for reads inside or crossing a 3 s observed PTY-size settling interval. Eligible ANSI responses can compose retained history without admitting the incoming page. Unsafe frames carry `resize_settling`; a leasing app bounds its display wait at 4 s. |

Compatibility matrix:

| App | Relay | Result |
| --- | --- | --- |
| ≤ 0.22.7 | this release | Sees no `pane_size_lease`, so never leases; a stale lease without `opt_in` is refused before any access. |
| this release | ≤ 0.22.7 or Stage A/removal builds | The relay lacks `pane_size_opt_in`, so the app never sends a lease. |
| this release | this release, darwin/linux | Leases only when the device opted in. |
| this release | this release, other OS | Capability absent; default layout, no error. |

The app gates leasing on all of: controller role, the relay's
`pane_size_opt_in` capability, and the device's Resize Session switch. Rows
additionally need `pane_size_opt_in_rows` and the Lease Terminal Height switch.
Every lease carries `opt_in: true`. The app records every pane it asked to
lease outside the terminal view, so turning the switch off sends an immediate
release for each of them, including panes whose view already closed and are
only in the navigation grace; a lease reply arriving after that is ignored.

## Lease model

Constants: `LeaseTTL` 120 s (rides out hidden-tab timer clamping),
`ReleaseGrace` 10 s, sweep every 1 s. Several clients on one pane get the
narrowest leased width and, if any client leases rows, the shortest leased
height. A width-only lease never changes the height: `TIOCSWINSZ` writes both
dimensions, so the unchanged one is written back with the value just read
through the same descriptor, and the pixel fields are always preserved.

At acquisition the current size becomes the baseline (restore point). During
an active lease, a dimension found changed since the last write was changed
locally and becomes that dimension's new baseline; the lease size is then
applied again.

## Identity boundary

At acquisition the relay captures a terminal identity:

1. pane ID;
2. Herdr's `terminal_id` from a fresh `pane.get` socket call (the agent
   inventory does not cover plain shell panes);
3. `shell_pid` from a fresh `pane.process_info` socket call; when Herdr reports
   no shell, `foregroundPID` selects the foreground group leader or a reported
   foreground process;
4. that process's controlling TTY path: Linux reads `tty_nr` and `starttime`
   from `/proc/<pid>/stat`; Darwin uses `ps -o tty= -p <pid>`;
5. the device number (`st_rdev`) from `fstat` of the opened TTY.

Linux's procfs resolver checks process birth and controlling-TTY identity
before and after inspecting the candidate `/dev/pts/<minor>` device. The
candidate must be a character device with the decoded device number. Shell
validation compares the recorded PID and start time. When Herdr reports no
shell, validation resolves the current foreground leader and checks its TTY
path and device instead; foreground-process turnover on that terminal is
permitted. These process fingerprints stay in memory.

A missing `terminal_id`, unavailable shell/foreground identity, or unavailable
TTY rejects acquisition before a size write.

Every mutation (acquisition, renewal, reconcile, restore, shutdown restore)
then follows the same sequence, implemented once in `openValidated`:

1. open the cached TTY path with `O_RDONLY|O_NOCTTY|O_NONBLOCK|O_CLOEXEC`;
2. `fstat` the held descriptor: it must be a character device with the
   recorded device number;
3. **then** re-validate freshly: `pane.get` reports the same `terminal_id`;
   `pane.process_info` retains the recorded shell identity, or still reports no
   shell for the foreground fallback; the selected process's controlling TTY
   must match the cached path and device evidence;
4. read and write the window size with ioctls on the held descriptor;
5. close it.

A successful check is never cached for a later write.

**Why open before validating.** A pseudo-terminal is not returned to the
allocator while any descriptor on it is open: Linux frees a devpts index only
when the last master and slave references are released, and XNU frees a ptmx
minor only once both its master and slave sides are closed. Opening first pins
the device instance. If the shell recorded at acquisition is afterwards still
alive on that TTY, the TTY was never released between the open and the check,
so the held descriptor is that shell's terminal and cannot be a reused PTY that
now belongs to another pane. Checking first and opening second would leave
exactly that window. The device number alone cannot detect reuse (a reused
index has the same device number); it catches a different device appearing at
the path.

`TestHeldSlaveDescriptorPinsPTYName` exercises device-instance pinning with
real PTYs. The Linux native `TestHeldSlaveSizeAfterMasterClose` also checks
that Size fails on a held slave after its master closes. That hangup test has
not run on Darwin for this change; Darwin history reads retain read-owned
descriptors rather than enabling the shared read pin.

**Outcomes.** A confirmed change (`pane_not_found`, different `terminal_id`,
`shell_pid` or TTY, different or non-character device, `ENOENT`/`ENXIO` on
open) drops the pane's lease state and any pending cleanup with no write and
one info log. Socket, procfs or `ps` failures, timeouts, and ioctl errors can
leave identity or geometry unverified. The failed operation writes no size;
pending cleanup retries within its budget.

## Cleanup budget

When a pane's last lease ends (immediate release, disconnect, grace or TTL
expiry) the pane enters restore-pending, modelled separately from active
leases with an attempt count, a next-attempt time and a fixed deadline:

- at most 5 attempts, all within 30 s of the moment the last lease ended;
- backoff 1 s, 2 s, 4 s, 8 s (capped at 10 s) after each failed attempt;
- waiting for a busy pane's lease lock and time spent in lookups, open and
  ioctls count against the 30 s; a delayed sweep keeps the original lease
  expiry as its start time, and an attempt whose next slot would fall past
  the deadline ends the cleanup;
- on exhaustion the state is dropped and one warning names the pane and
  advises a local resize. Nothing retries forever.

Renewals and failed lease requests never extend or reset a pending cleanup.
A successful new acquisition supersedes it: the original baseline is kept, and
the pane's generation is incremented so a cleanup attempt planned earlier can
never write over or delete the newer lease.

Lease acquisition, reconciliation, and restore work use a per-pane lock.
History reads validate held descriptors separately and check geometry revisions
around their samples. Bookkeeping uses a separate short lock. Each cleanup
attempt runs with a 3 s timeout, and the sweep handles due panes concurrently.
Shutdown stops accepting leases and makes one
concurrent restore attempt per pane within a 5 s overall deadline; panes it
could not restore are logged and returned as an error, never reported as
restored.

## Restore per dimension

At restore time the current size is re-read through the validated descriptor.
For each dimension, a value that differs from the last one the lease applied
was changed locally and is kept; otherwise the baseline is written back. A
dimension the lease never changed is never written with a different value.

This is a best-effort heuristic based on value equality, not a guarantee that
a local change is never overwritten: a local resize back to exactly the leased
value (ABA) is indistinguishable from no change, and a local resize landing
between the relay's read and its write can be overwritten, because there is no
compare-and-set for the window size.

## Sampled fullscreen history

The relay retains sampled physical pages for Claude-like agents while its
pane-wide lease gate is active. `AgentState.UsesNoScrollbackHistory` selects
names containing `claude` or `qoder` with a known zero
`max_offset_from_bottom` metric. Polls join fresh `pane.list` metrics by pane
and nonempty terminal identity; event topology commits retain current
same-terminal evidence. Missing metrics do not qualify. Zero describes a lack
of scrollback, not an explicit alternate-screen mode: a fresh primary-screen
pane can satisfy this heuristic.

`LeaseHistoryState` includes resolved lease state, navigation grace, expired
leases awaiting cleanup, and pending restore work. The gate ends when cleanup
drops that state. A lease owned by another relay does not enable this relay's
physical source. Unleased legacy reads perform no TTY open, Size sample,
resolver call, or geometry identity lookup. They do not observe another
relay's PTY-size changes. This relay's restore settling remains fenced through
in-memory geometry state.

While the gate and heuristic qualify, display, background, and completion
fallback captures request ANSI `recent` with `PhysicalCaptureLines=1000`.
A non-leasing reader of the same pane uses that source too. Reader preferences
of 100, 500, 1,000, or 10,000 rows limit the response after archival; they do
not change the physical capture shape. `MaxLines=10000` bounds history rows.
A separate `MaxHeldRows=2000` ring stores unresolved physical rows, beyond
that history limit. Held rows are not shown on the phone.

Before lease activation and after restore cleanup, Claude reads return to
`ReadPane` (`recent-unwrapped`) and the legacy active-segment matcher. Qoder
keeps its existing display-source selection outside the physical route.
Inline or unknown-metric viewport responses remain raw. Prior physical
segments stay frozen when a later legacy segment is matched.

### Boundaries, matching, and the current page

Physical ANSI reads use `BeginHistoryRead` and `Finish` to sample Size around
the content read. On Linux, the read borrows a manager-owned descriptor while
the pane-wide lease or restore interval retains it. Cold resolution uses fresh
identity lookup and post-open validation. Warm borrowing adds no read-triggered
identity lookup, resolver call, reopen, or device-stat check. Lease mutations
continue to use their separate, freshly validated write descriptors.

Finish, Close, and cancellation release a read's borrow. Size failure, fresh
validation failure or identity mismatch, observed departure through
`ForgetHistoryPane`, terminal replacement, gate-off plus settle expiry, and
shutdown retire the matching pin. Closure waits for its borrowers to release.
The manager retains at most one history pin per pane, including a retired pin
that is still draining. If a borrower's Size call does not return, later
physical captures of that pane remain refused; shutdown reports unresolved
history descriptors at its deadline.

A held descriptor identifies a PTY instance, not the runtime Herdr currently
serves. Herdr can respawn a shell on a new PTY without changing `terminal_id`.
A Linux read can borrow the old pin until the first Size failure, fresh
validation signal, or accepted departure invalidates it.
If its Size still succeeds and coordinator eligibility has not changed, the
existing checks cannot distinguish the old PTY from the new runtime. This
window has no fixed detection bound. No per-read process polling or new runtime
reset callback is added.

Darwin keeps the read-owned descriptor path until native master-close/Size
failure qualification permits a pin. A warm read uses its cached identity and
post-open validation; cold reads and invalidation recovery perform identity
lookup. Finish, Close, and cancellation close that read-owned descriptor. The
Darwin native hangup test has not run for this change. Background deferral
checks use retained geometry without opening another handle.

The epoch tracks observed PTY rows and columns and fences unsafe read
intervals. It does not determine physical segment continuity at equal known
columns. `AdmitSnapshot` continues legacy matching across resize, restart, and
lease revision, including known-zero no-scrollback panes. Equal positive
physical columns continue matching across height, epoch, lease revision, and
restart changes. Source changes, changed or unknown physical columns, and
invalid provenance start representation boundaries. Terminal replacement
resets the pane state.

At a supported boundary, `matchBoundarySuffix` compares ordered whitespace
words after ANSI stripping and NBSP equivalence. Agreement must cover at least
12 words over two substantive archive rows. It removes one contiguous proved
archive suffix at whole-row edges before installing the incoming raw page.
Non-substantive layout between proved rows can join that suffix. If the new
page begins partway through an archive row, the matcher retains that partial
seam row and removes the proved rows after it.

Reconciliation reads only rows of the departing segment, within the budgets below. Its budgets are 1 MiB of raw inspection, 256 KiB of
normalized input, 16,384 tokens, 64 candidate seams, and 65,536 token comparisons.
Raw-length checks precede normalization and comparison-view allocation; a
second raw normalization pass is charged separately. Exhaustion, unsupported
structure, or occurrence ambiguity refuses the cut and retains the boundary
copy. Code/table structure, split paths or URLs, split words, repeated prose,
and competing occurrences can cause refusal. Comparison tokens do not
reconstruct the displayed page or split stored ANSI rows.

Previously frozen physical prefixes stay outside destructive legacy matching.
An external A→B→A change entirely between physical samples remains undetectable.
Emulator-grid or pixel-only changes do not advance the sampled epoch. Narrowing
Herdr's grid below the PTY width can clip rows without a detected geometry
change; the shared desktop-garbling limitation remains.

Inside a physical segment, ordered matching compares substantive rows: rows
containing a Unicode letter or digit. Repeated text must agree on its occurrence
across optimal ordered mappings before it can anchor retirement. Supported
forward ranges retire in old-coordinate order, with their interior blank layout.
A stationary header can remain in the current page without entering that range.
A supported same-size interior slot cleared to blanks is archived before
replacement rules apply. Comparisons ignore trailing blank padding and equate NBSP with spaces;
stored rows retain their raw text and ANSI styling.

`ReplaceableTailLines=3` bounds in-place replacement of a supported old slot or
tail of one to three physical rows, including intervening blanks and punctuation.
The corresponding new slot must contain at least as many physical rows. Previous
values are not archived; this exception can replace incomplete transcript text.
Equal slot edges remain matched before the changed middle is classified.

Unexplained contraction, disputed occurrences, and reverse-pushed rows become
held candidates before the discontinuity check. A verified return of the
current segment's archived suffix retracts that prefix from the displayed archive.
One substantive returned row can qualify under two agreed surviving anchors and
the combined occurrence proof. A zero-displacement cleared-slot return uses one
runtime record: it binds the archived tail to its segment, archive generation,
agreed neighbours, coordinates, and intervening layout. The slot must remain
blank under that support until the rows return. Successful retraction consumes
the record; archive mutation, trim, reload, terminal reset, or a representation
boundary invalidates it. This is not global text deduplication and does not
remove duplicates already stored. Same-segment matching does not search earlier
segments. The response is the archive prefix
followed by the raw current page, including its header, footer, and styles,
before the reader's row limit and response clipping apply.
Held rows remain invisible. Their separate 2,000-row ring removes oldest rows in
append order, including partial records, with eviction counts in logs. History
prefix trimming can also expire held records whose archive pins fall before it.
Rows that reappear or retire in a later admission can keep their earlier held
copies. Those copies remain in the 2,000-row ring and consume its capacity.

A matcher discontinuity freezes the old body once and installs the new raw page.
The `held_majority` reason requires `H >= 3 && 2*H > N && N >= 8`, where H is newly
held old nonblank rows and N is old nonblank rows. Matched, replaced, and retired
rows remain explained in N; retiring most of a page during a fast scroll is not
itself a freeze trigger. The separate `pair_budget` reason applies above
`MaxPhysicalMatchPairs=65536` equal substantive pairs when linear matching cannot
certify the transition. Short `/clear` exchanges can be replaced or held under
the generic rules, so a clear does not promise visible retention by command name.
Larger unexplained clears can reach the held-majority freeze threshold.

The relay logs a reason, counts, and held share for a matcher freeze, without row
text. Per-pane minute counters report admission classes, retirements, replacements,
held additions/evictions/expiry, current held size, reverse retractions, and pair
work. Boundary admissions are counted separately from matcher freezes.
`boundary_reconciled_rows`, `boundary_refused`, and `boundary_seam_kept` count
boundary results for physical and legacy destinations. Shared-read followers
reuse the owner's admission and do not add another boundary count.

`FooterLines=6` splits the replaceable footer when a page exceeds six rows.
Taller chrome can enter retained history; shorter chrome can leave transcript
rows in the replaceable footer. Footer updates refresh even when the body is
unchanged. Blank rows remain content rather than being collapsed. This does
not establish the cause of a blank run or recover text absent from a sample.

Herdr 0.9.5 supplies placeholder `pane.read.revision=0`, not usable content
order. The relay sequences underlying reads at start and shares the owner's
admission outcome with followers. An older completion cannot replace a newer
archived read. Reads that sample content in a different order use the same
accounting rules: unresolved rows remain held unless a discontinuity freezes
the old body. Verified current-segment overlap can retract the archive prefix.

The relay skips archival inside or across the three-second settling interval.
Rows appearing and disappearing during that pause are lost. Bursts larger
than the sampled page between captures can also leave gaps. The background
service checks on a four-second tick. Its interval accounting uses scheduling
time rather than validation completion time. The scheduler starts captures of
one pane at least three seconds apart, so small timer jitter does not skip a
tick and a late tick does not add a second capture. Inflight work, settling, and
scheduler delays can still defer a capture. Trailing requests add sampling
opportunities.

### Response clipping, saving, and app compatibility

Composed responses obey a JSON-encoded budget of 4 MiB minus 64 KiB, in addition
to the reader's row limit. `fitPaneContentResponse` drops oldest response rows
first, then current-page rows if needed. It keeps whole rows and reports
`truncated`; if no row fits, it returns empty successful content with
`truncated=true`. Response clipping does not delete stored history. This byte
guard also applies to large unleased legacy responses; fitting legacy output
stays unchanged. A single composed frame no longer exceeds the per-message
limit. This does not reserve space in the shared transport queue or change its
eviction policy; a nonempty queue can still reject a frame.

The truncation notice is not a completeness check: missing samples, settling
loss, footer/slot/tail replacement, invisible held rows, and retention limits
can omit displayed text without a new notice. A narrow cosmetic limit remains for direct reads: if byte clipping
changes `truncated` but leaves content identical, the previous notice can remain
until a content change or watch frame delivers updated metadata.

Accepted departures from `working` or `blocked` queue a trailing terminal
capture, serviced on the four-second tick. Requests retain a 60-second lifetime
and a five-actual-attempt bound. Native Conversation responses do not clear a
separate pending terminal request. Dirty history has a scheduled save deadline:
the first save is due immediately, later changes are due at the last successful
save plus `SaveInterval=10s`, and write failures retain dirty state for retry.
Shutdown closes admission, joins capture/persistence work, and saves dirty
state without waiting for its deadline. Clean loaded caches remain unchanged.
A crash before a successful save can still lose recent samples.

`snapshot_version=1` stays unchanged. Valid legacy state resumes its active
segment after restart, including known-zero panes and valid v0.22.10 flat
files, which upgrade in place. Valid physical state continues when stored and
incoming columns are equal and positive. Missing physical width or invalid
metadata retains a conservative boundary; invalid provenance cannot authorize
suffix deletion. Source changes remain boundaries across restart.

Archive generation, the cleared-slot return record, comparison views, and
history descriptor ownership stay in memory. They add no saved JSON field.
The shared pane-ID cache layout is unchanged; separate relays still need
separate cache roots to avoid shared-cache interference.

Legacy matching can accept a blank-row tail overlap before trying a substantive
sequence match. If blank padding matches just the incoming first row,
`mergeLegacyBody` keeps the archived prefix and appends the incoming rows after
that blank, which can repeat transcript content. This pre-existing limit is
unchanged by this release.

This release includes a new phone-app build. It estimates leased prose using
the effective shared width and adjusts short/blank anchors for contextual
prepends. Use the matching app build; published 0.22.11 and older apps lack
these frontend corrections. No new history capability or
client-side history merger is introduced.

## Residual risks

- **Garbled shared view while leased** — inherent, disclosed in Settings.
- **Scrollback damage** — agents redraw on every size change and may leave
  narrow copies of their output, or stale status bars after height changes,
  in the scrollback. Resizing may also truncate scrollback and lose older
  output. It cannot be cleaned up afterwards.
- **Value-equality restore heuristic** — see above.
- **Desktop fights** — when the desktop resizes the pane while it is leased,
  the change becomes the new restore point and the lease is applied again at
  the next renewal or reconcile, so the shared size can flip back and forth.
- **Identity residuals** — mutation validation trusts Herdr's pane/process
  reports and the platform's controlling-TTY evidence. Linux checks shell
  process birth; Darwin retains its `ps` path. The Linux read pin can remain
  stale after same-terminal-ID respawn until a documented invalidation signal.
  A retired pin with a blocked borrower can keep that pane's later physical
  captures refused; shutdown reports the unresolved descriptor.
- **Platforms** — darwin and linux only; other builds do not advertise it.
- **Phone qualification incomplete** — unit, real-PTY, relay blackbox and
  browser tests do not establish an end-to-end real-phone resize/restore pass.

## Follow-up work

**Coordinated resizing needs Herdr.** The proper contract is an absolute
rows/columns operation owned by Herdr that updates grid and PTY together, is
scoped to a terminal identity (`PaneInfo.terminal_id`), defines multi-client
constraints, release/expiry/restart semantics, and a resize revision for
conditional restore. Until it exists, the opt-in direct-PTY path described here
is the only resizing on offer. Verify the running server's protocol before
building on such an API; the installed CLI version is not capability evidence.

**Phone cursor** is not rendered: `pane.read` returns text without a cursor
position and the app draws no cursor. Key delivery works. This is separate from
resizing.
