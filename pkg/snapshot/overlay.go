package snapshot

// OverlayOptions mirror the options of containerd's built-in overlayfs plugin.
type OverlayOptions struct {
	SyncRemove   bool
	MountOptions []string
}
