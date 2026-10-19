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
3. `shell_pid` from a fresh `pane.process_info` socket call; only when Herdr
   reports no shell is the foreground process-group leader used instead;
4. that process's TTY path, from `ps -o tty= -p <pid>` (validated to stay
   under `/dev`);
5. the device number (`st_rdev`) from `fstat` of the opened TTY.

A missing `terminal_id`, shell or TTY rejects the acquisition with a clear
error and no write.

Every mutation (acquisition, renewal, reconcile, restore, shutdown restore)
then follows the same sequence, implemented once in `openValidated`:

1. open the cached TTY path with `O_RDONLY|O_NOCTTY|O_NONBLOCK|O_CLOEXEC`;
2. `fstat` the held descriptor: it must be a character device with the
   recorded device number;
3. **then** re-validate freshly: `pane.get` reports the same `terminal_id`,
   `pane.process_info` the same `shell_pid`, and that pid's TTY is still the
   cached path;
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

`TestHeldSlaveDescriptorPinsPTYName` checks the pinning empirically with real
PTYs: on macOS a held `/dev/ttysNNN` was not handed to any of eight new PTYs
and was reused by the very next allocation after it was closed. The same test
runs on Linux, where it has not yet been executed for this change.

**Outcomes.** A confirmed change (`pane_not_found`, different `terminal_id`,
`shell_pid` or TTY, different or non-character device, `ENOENT`/`ENXIO` on
open) drops the pane's lease state and any pending cleanup with no write and
one info log. Anything else (socket or `ps` failures, timeouts, ioctl errors)
is unknown: nothing is written, and only a cleanup retries, within its budget.

## Cleanup budget

When a pane's last lease ends (immediate release, disconnect, grace or TTL
expiry) the pane enters restore-pending, modelled separately from active
leases with an attempt count, a next-attempt time and a fixed deadline:

- at most 5 attempts, all within 30 s of the moment the last lease ended;
- backoff 1 s, 2 s, 4 s, 8 s (capped at 10 s) after each failed attempt;
- waiting for a busy pane's I/O lock and time spent in lookups, open and
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
physical source. Its observed PTY-size changes can still create a geometry
boundary and a settling pause.

While the gate and heuristic qualify, display, background, and completion
fallback captures request ANSI `recent` with `PhysicalCaptureLines=1000`.
A non-leasing reader of the same pane uses that source too. Reader preferences
of 100, 500, 1,000, or 10,000 rows limit the response after archival; they do not
change the physical capture shape. The retained body is bounded by `MaxLines=10000`.

Before lease activation and after restore cleanup, Claude reads return to
`ReadPane` (`recent-unwrapped`) and the legacy active-segment matcher. Qoder
keeps its existing display-source selection outside the physical route.
Inline or unknown-metric viewport responses remain raw. Prior physical
segments stay frozen when a later legacy segment is matched.

### Boundaries, matching, and the current page

`BeginHistoryRead` and `Finish` sample a held, validated TTY descriptor around
the content read. This validation also runs for eligible unleased ANSI reads;
text and legacy viewport-only reads skip it. Background deferral checks use
retained geometry without opening another handle.

The epoch tracks observed PTY rows and columns. Source changes start fresh
history segments for each pane. Lease changes, observed PTY changes, and relay
restarts start segments for physical and known-zero no-scrollback contexts.
Panes with scrollback keep legacy matching across leases, PTY resizes, and relay
restarts. Previously frozen physical prefixes remain frozen. A failed geometry
sample does not fabricate an epoch-zero boundary. A fullscreen boundary can
repeat one page. An external A→B→A change entirely between samples remains
undetectable. Emulator-grid or pixel-only changes do not advance this epoch.
Narrowing Herdr's grid below the PTY width can clip rows without a detected
geometry change; the shared desktop-garbling limitation remains.

Inside a physical segment, accepted fixed-header alignment archives displaced
scroll rows. The response places that archive before the raw current page,
including its current header, footer, and ANSI styling. The header is not added
to the displaced region. Comparisons ignore trailing blank padding, but stored
and displayed pages preserve raw rows. A separate `ReplaceableTailLines=3`
policy permits up to three rows at the content tail to change in place, before
that padding. Those rows can contain a spinner, wrapped tips, or incomplete
transcript text; their old values are not archived. Candidates with identical
ordered displaced rows can share one outcome. Blank-only overlap cannot remove
rows. Larger divergent tails and conflicting or reverse alignments retain
the previous page and append the incoming page, which can repeat content even
at unchanged PTY geometry.

`FooterLines=6` splits the replaceable footer when a page exceeds six rows.
Taller chrome can enter retained history; shorter chrome can leave transcript
rows in the replaceable footer. Footer updates refresh even when the body is
unchanged. Blank rows remain content rather than being collapsed. This does
not establish the cause of a blank run or recover text absent from a sample.

Herdr 0.9.5 supplies placeholder `pane.read.revision=0`, not usable content
order. The relay sequences underlying reads at start and shares the owner's
admission outcome with followers. An older completion cannot replace a newer
archived read. Reads that sample content in a different order can still reach
the conservative reverse/ambiguity fallback and repeat a page.

The relay skips archival inside or across the three-second settling interval.
Rows appearing and disappearing during that pause are lost. Bursts larger
than the sampled page between captures can also leave gaps. The background
service checks on a four-second tick. Its interval accounting uses scheduling
time rather than validation completion time. Inflight work, settling, and
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
loss, footer/tail replacement, and retention limits can omit text without a
new notice. A narrow cosmetic limit remains for direct reads: if byte clipping
changes `truncated` but leaves content identical, the previous notice can remain
until a content change or watch frame delivers updated metadata.

Accepted departures from `working` or `blocked` queue a trailing terminal
capture, serviced on the four-second tick. Requests retain a 60-second lifetime
and a five-actual-attempt bound. Native Conversation responses do not clear a
separate pending terminal request. Dirty history has a scheduled save deadline:
the first save is due immediately, later changes are due at the last successful
save plus `SaveInterval=10s`, and write failures retain dirty state for retry.
Shutdown closes admission, joins capture/persistence work, and saves admitted
state. A crash before a successful save can still lose recent samples.

Physical and no-scrollback history becomes a frozen prefix at its first
runtime admission after restart, even if numeric PTY epochs and lease revisions
repeat. Scrollback legacy reads resume matching, including older-format files.
Restart fencing does not infer the source or width of rows from older files.
The shared pane-ID cache layout is unchanged; separate relays still need
separate cache roots to avoid shared-cache interference.

This release includes a new phone-app build. It estimates leased prose using
the effective shared width and adjusts short/blank anchors for contextual
prepends. Use the matching app build; the older 0.22.10 app can receive long
frames but lacks these frontend corrections. No new history capability or
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
- **Identity residuals** — the check trusts Herdr's `terminal_id` and
  `shell_pid` and `ps`'s TTY name. Defeating it would need the shell to exit
  and its pid to be reused by a process on a reused PTY with the same name,
  while Herdr still reports the old terminal for the pane. The pinning claim
  rests on the kernel behavior above, measured on macOS only so far.
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
