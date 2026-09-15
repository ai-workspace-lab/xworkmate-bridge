# Remote desktop extraction

The WebRTC remote-desktop implementation under `internal/desktop` has been
copied to `ai-workspace-lab/xworkmate-remote-desktop` as an extraction
baseline. This copy does not deprecate or remove the implementation here.

Until the standalone server has a stable signaling API, packaging, deployment,
and production validation, this repository remains the authoritative runtime.
Future migration will replace direct desktop ownership with a client or proxy
to the standalone service. The long-term boundary is that `xworkmate-bridge`
has no X11, capture, encoder, or input-injection dependency.
