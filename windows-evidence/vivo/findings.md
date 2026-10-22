# Windows Remote PC investigation — 22 October 2026

Two successful sessions were captured from existing vendor logs, without modifying the vendor application. The operator confirmed that the desktop displayed and could be controlled, then explicitly reported “reconnected”. The exact instant the phone left its connecting screen was **not measured**.

The strongest actionable findings are:

1. Windows sends screen state over cloud WebSocket signaling in addition to negotiating WebRTC. `screenLockInfo` occurs immediately after `open`; `deviceScreenInfo` is generated on the SIP INVITE path and the phone acknowledges it with `action:"confirm"`, the matching `msgId`, and `data.type:"deviceScreenInfo"`.
2. Windows actually sends H264 PT 98, `profile-level-id=42e01f`, `packetization-mode=1`, with RTX PT 99. Its first paced keyframe is logged 109 ms after data-channel opening in session 1 and 62 ms after opening in session 2.
3. On reconnect, the first paced keyframe precedes receipt of the screen-info confirmation by 143 ms. **Windows does not gate video sending on that confirmation.** This does not establish whether the phone waits for screen information, video decode, or both before changing its UI.
4. The selected route is direct host-to-host UDP over IPv6, despite vivo relay candidates being offered. A vivo relay is not necessary for these successful sessions.

The Linux implementation was not present in this initially empty workspace. The implementation priorities below are comparisons to the user's description, not findings from inspecting its source or traffic. A definitive missing-handshake-versus-incompatible-video diagnosis remains open.

## Environment and coverage

| Item | Evidence / result |
|---|---|
| Windows | Registry reports 24H2, build **26100.4652**, ProductName `Windows 10 Home Single Language`. ProductName and build naming are inconsistent; WMI OS-caption queries were denied. Preserve the build as the reliable recorded identifier. |
| Office Kit | **7.0.6**, installed application registration and executable version. |
| Remote PC UI | **3.0.6**, executable and renderer log. |
| Remote service | Service logs identify **3.0.4**; `v_remote_service.exe` file-version resource is **14.20.0**. These are separate version sources. Dependency minorVersion: **20250730**. |
| Phone | Model, firmware/OriginOS and phone component version were requested but not supplied. No ADB device was attached. |
| Same LAN | Selected Wi-Fi IPv6 host candidates support direct connectivity. This does not independently establish that both devices use the same LAN; operator did not supply the topology. |
| Account requirement | Not established. A successful session alone cannot distinguish previously cached login from code/password-only support. No account/security settings were changed. |
| Operator tests | Operator replied “done” to the requested changing-window/input/60-second exercise and later “reconnected”. Individual shortcut/text outcomes were not described. |
| Independently logged inputs | Pointer movement, scrolling, left/right actions and keyboard injection. First mouse injection: **15:09:44.611**; first keyboard injection: **15:12:17.476**. Injection logs establish PC-side processing, not independently observed visual results of every action. |

The WinForms changing-text/input harness was launched, but its saved timeline contains no desktop-visible marker or input events. Its UI visibility and changing-text test are therefore unverified. Its original timeline is retained privately. The useful session evidence comes from vendor logs and direct log snapshots, not from that harness.

## Correlated timeline

All times below are the **PC local log clock, UTC+05:30**, on 2026-10-22. Millisecond precision is log precision, not a claim of clock accuracy. The cloud envelope clock differs substantially: the service logs a server-time correction of roughly 19,799,463 ms. Do not align embedded cloud `timeStamp` values directly with PC log times.

| Stage | Session 1 | Reconnect / session 2 |
|---|---|---|
| Cloud `open`, phone → PC | 15:09:23.456 | 15:16:56.424 |
| `screenLockInfo`, PC → phone, unlocked | 15:09:23.462 | 15:16:56.432 |
| `openSuccess` send logged | 15:09:23.525 | 15:16:56.480 |
| First captured frame, service callback | 15:09:24.892 | See timeline.jsonl |
| SIP INVITE in native SIP log | 15:09:25.535 | 15:16:58.077 |
| SIP 100 / 180 | 15:09:25.536 / .536 | See sanitized SIP excerpt |
| `deviceScreenInfo` confirmation received | 15:09:26.530 | 15:16:58.834 |
| Native local answer before encryption | 15:09:26.179 | See timeline.jsonl |
| Selected ICE pair | 15:09:26.241 | 15:16:58.129 |
| SIP 200 OK, native log block time | 15:09:26.435 | See sanitized SIP excerpt |
| SIP ACK | 15:09:26.761 | 15:16:58.539 |
| DTLS handshake complete | 15:09:26.762 | 15:16:58.625 |
| Data channel open | 15:09:26.789 | 15:16:58.629 |
| SCTP state 2 | 15:09:26.796 | See timeline.jsonl |
| First received binary heartbeat | 15:09:26.796, type 254, size 5 | See timeline.jsonl |
| First paced keyframe | **15:09:26.898** | **15:16:58.691** |
| First-frame-sent service callback | 15:09:26.940 | 15:16:58.906 |
| First logged mouse injection | 15:09:44.611 | See timeline.jsonl |
| SIP BYE | 15:13:03.584; retries follow | Not required to terminate second session |

Session 1 therefore remained connected for approximately **217 seconds** after data-channel opening. Existing vendor logging began before the first `open`; raw snapshots retain that history. The harness start was 15:07:46, but its log-snapshot loop did not produce usable files; the independent collector preserved vendor logs afterward. There is no screen recording or packet capture.

Service callbacks arrive later than native events: for example service “SDP create” timestamps are later than native `SetLocalDescription`. Prefer native subsystem logs for transport timing. SIP message times are the preceding native log-block timestamp, not packet-capture timestamps. The timeline includes pre-session REGISTER/OPTIONS 200 responses and labels them as potentially unrelated.

## Application messages: observations versus code evidence

| Message | Successful-session evidence | What can be concluded |
|---|---|---|
| `screenLockInfo` | Service logs send parameters with `isLock:false` in both sessions, before `openSuccess`. | Actual startup state notification. Whether the phone requires it is unproven. |
| `deviceScreenInfo` | Screen enumeration and local screen update on INVITE; matching phone `confirm` in both sessions. | Its occurrence is strongly supported by the acknowledgement and send-path code. The outgoing wire bytes are not logged. |
| `getDeviceScreenInfo` | No corresponding startup request identified in session logs. | Do not require the phone to ask first. Installed service pushes screen information from its INVITE handler. |
| `deviceScreenDpiInfo` | Session 1 has a native DPI callback logged at 15:09:24.974 with 1920×1080, 125%; source code sends this notification from that callback. | DPI value **1.25**, not 125, in the reconstructed message. Outgoing payload itself is not captured. |
| `accountList` | No account-list payload or confirmation identified. Code calls the check, logging unlocked state and connectionType 0. | Code sends the list only for `needAccountAuth==1 && connectionType==1 && isLock`; this ordinary remote-control session does not meet those conditions. |
| `accountAuthResult` | No account-auth-result exchange identified. | Not established as a startup requirement for this unlocked remote-control flow. |
| `clientConnectSuccess` | Logged before the remote session; installed handler responds by SIP registration and status synchronization. | A cloud connection/recovery event handled by PC; no evidence it is a PC→phone “desktop ready” message. |
| Capture/permission readiness | Code invokes a permission-check helper after `openSuccess`; capture and encoder callbacks occur. | No mandatory phone-facing permission/capture-ready acknowledgement was identified in these sessions. |
| `confirm` | Phone→PC screen-info confirmations occur in both sessions. | Code stops retrying when matching `msgId` is confirmed. Session 2 proves the sender can emit video before this confirmation returns. |
| Initial JSON on data channel | Existing JSON callback suppresses ordinary mouse/key/widget logging. | Startup JSON contents/order are unresolved; an absence in these logs is not evidence of no JSON handshake. |

The installed service code constructs WebSocket envelopes with `action` defaulting to **`msg`**, parameters under **`data`**, and `sourceDeviceNo`, `targetDeviceNo`, `connectionId`, `msgId`, `timeStamp`, and `version:"v1"`. Screen-info `currentScreenIndex` is a display **id**. The observed display is id/index 0, selected, 1920×1080, origin 0,0.

See `evidence/startup-payloads-synthetic.json` for shareable structures. Its outgoing envelopes are reconstructed from code and logged parameters, not represented as raw captured packets. `deviceScreenInfo` retries are keyed by `deviceScreenInfo-confirm-<msgId>`; account confirmations use per-type flags. Retry helper defaults to three attempts, but its exact default interval was not established here.

## SDP and video

`offer-redacted.sdp` is the first native **decrypted** offer. `answer-redacted.sdp` is the first native local answer **before fingerprint encryption** and before candidate gathering is complete. `answer-gathered-redacted.sdp` preserves the later updated local answer structure/candidates; its fingerprint has also been replaced. All credentials, fingerprints, candidate foundations, non-wildcard addresses, session ids and SSRC values are synthetic. Media order, MIDs, directions, codec parameters, extension ids, feedback, SSRC relationships, MSID strings, candidate types/priorities and SCTP parameters are preserved. These are comparison artifacts, not replayable authenticated SDP.

| Property | Observation |
|---|---|
| Media / BUNDLE | audio, video, application; `a=group:BUNDLE audio video data`. |
| Directions | Phone audio/video `recvonly`; Windows audio/video `sendonly`. |
| DTLS | Offer `actpass`; Windows answer `active`. Windows initiates DTLS. |
| Negotiated video | Native `Negotiated codec: VideoCodec[98:H264]`, encoder factory H264 and OpenH264 2.1. SDP also retains H265; its presence does not mean it was used. |
| H264 | PT 98, `level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f` (constrained-baseline signaling, level 3.1). Actual SPS profile/level was not independently parsed. |
| RTX/FEC | H264 RTX PT 99 apt=98; H265 PT127/RTX97 apt127; RED100/RTX101 apt100; ULPFEC102. FlexFEC35 offered, omitted in answer. Runtime says ULPFEC disabled with this H264/NACK combination. |
| Feedback / extensions | NACK, PLI, FIR, transport-cc, goog-remb and rrtr; full negotiated extension ids/URIs retained in SDP artifacts. |
| Streams | Answer has audio and video stream/track MSIDs; video FID group links primary and RTX SSRCs. |
| SCTP | `UDP/DTLS/SCTP webrtc-datachannel`, MID data, port 5000, `max-message-size:262144`. |
| Connectivity | Host and vivo relay candidates available. Native selected pair is host↔host IPv6 UDP; service reports p2p. |
| Keyframe | Native pacer explicitly logs sending keyframes at .898 and 15:09:27.142. Service first-frame-sent follows at .940. |
| SPS/PPS/IDR | NAL contents are not logged. No independent proof of their exact timing or the first keyframe's actual NAL composition. The pacer keyframe event is stronger than transport CONNECTED, but does not replace NAL inspection. |

Capture starts before INVITE in session 1. The visible code path does not require a resume-video command for ordinary `connectionType:0`; file-transfer mode 1 explicitly disables media. `changeWidgetState` / `multimediaTransfer` controls exist (0 pause video, 1 resume video, 2 pause audio, 3 resume audio), but **their occurrence as startup commands was not observed**. Video send state changes to enabled around channel opening. The exact native internal prerequisites have not been instrumented.

## Data channel

Runtime confirms **SCTP sid 100** receiving DATA and channel-open state 1. The installed RTC DLL contains the exact label `vivodatachannel`. Label selection in this particular connection, externally-negotiated mode, and ordered/retransmission settings are **not independently exposed by the logs**; retain the user's current settings as hypotheses until configuration is inspected.

Both **254 (0xFE)** and **255 (0xFF)** are received, each size **5**, on approximately one-second cycles. The first FE is .796 and first FF .842. This is consistent with heartbeat requests/replies in both directions. The four remaining bytes are not logged, so identical-tail echo is not verified. The configured heartbeat **15** is a timeout setting, not the observed one-second interval. A native heartbeat-mask message exists, but its value is omitted.

The first logged outgoing buffered sizes are 5, 309, 5. Native cursor-send size 290 accompanies the 309-byte send; this suggests framing/metadata overhead but does not establish its layout. Installed code handles binary type 0 as file data and sends cursor buffers through type 1. It parses ordinary channel messages with `JSON.parse(msg)` and routes `res.data.type` values `mouse`, `key`, `changeWidgetState`, and `fileTransfer`. Full binary framing and unlogged startup JSON remain unresolved.

## Prioritized Linux work and discriminating tests

1. **Compare/push screen metadata now.** If absent, implement the `action:"msg"` / `data.type:"deviceScreenInfo"` envelope on the INVITE path, with a stable message id, correct current display id, display dimensions/selection and bounded retries until matching `confirm`. Send truthful `screenLockInfo` after `open` and correct fractional DPI. These are actual Windows behaviors omitted from the described Linux milestones, not proven UI gates.
2. **Prove first usable encoded video.** Log H264 encoder output and RTP payloader output, first SPS/PPS/IDR, first RTP packet and remote feedback. Match PT98, packetization-mode1, profile signaling 42e01f, and send an immediate IDR with parameter sets after media becomes writable. Compare the Windows ~60–110 ms channel-open-to-keyframe baseline. CONNECTED alone does not prove a decodable stream.
3. **Diff the provided SDP structurally.** Preserve offer media/MID/BUNDLE ordering and offered PT mappings, sendonly answer direction, active DTLS role, SCTP parameters and coherent SSRC/MSID/FID information. Do not hard-code RTX when it was not negotiated or assume H265 because it remains in SDP.
4. **Verify the channel, not only WebRTC state.** Demonstrate sid100 OPEN and bidirectional FE/FF traffic with the required tail behavior. Keep the label/mode/ordering assumptions explicit. Do not treat a 15-second timeout as the send interval. Log application JSON before input dispatch so a missing startup exchange can be distinguished from absent video.
5. **Run controlled Linux comparisons.** First send screen metadata with the existing video pipeline; then correct video while keeping metadata fixed. Timestamp the phone UI transition, channel OPEN, first IDR/RTP, phone confirm and first input. This tests causality without assuming every Windows message is required.
6. **Only then broaden transport work.** A vivo relay is unnecessary on the observed direct path. Account-list/auth flows are conditional and lower priority for the demonstrated unlocked ordinary remote-control case.

## Evidence, limits and next capture

Shareable artifacts: `timeline.jsonl`, the three redacted SDP files, `evidence/environment.json`, synthetic startup examples, and **selected** sanitized excerpts under `evidence/sanitized/`. Each excerpt/timeline event identifies its raw source and line number. Raw source files may grow; the numbered snapshots preserve the referenced content. The timeline includes individual input classifications and heartbeats while omitting key payloads.

Original logs and manifests: `evidence/private/logs-20261022T094724Z/` (final reference snapshot), plus the earlier collector snapshot directory. Manifests record observation times, sizes and SHA256. Existing native sources: `%APPDATA%\pcsuite\log\vivocontrol\service`, `rtc`, `main`, `render`, and `watch`. Read-only extracted code is under `evidence/private/source/`; its `service-module-1.js` contains WebSocket handling, `service-module-3.js` RTC callbacks/screen-info/channel dispatch, `service-module-5.js` envelope construction, and `service-module-6.js` input/video controls. Module2/4 duplicate 1/3. Private drafts and maps are **not shareable sanitized evidence**. `.gitignore` excludes the entire private directory.

No vendor patch, runtime hook, TLS-verification change, firewall change, account change, phone setting change or vendor restart was performed by this investigation. The operator drove the successful sessions. Existing logs already exposed decrypted SDP and native media timing, so runtime instrumentation was unnecessary for those findings. Exact unlogged channel payloads, NAL bytes and phone UI timing still require a narrowly scoped follow-up capture; existing packet encryption cannot establish them.

The remaining decisive measurement is synchronized phone UI timing plus pre-dispatch JSON/channel configuration and encoded NAL metadata. Without it, asserting that one specific application acknowledgement makes the phone display the desktop would overstate the evidence.
