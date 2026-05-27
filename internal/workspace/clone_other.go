//go:build !darwin

package workspace

// cloneTree clones srcRepo into destPath. On non-Darwin platforms there is no
// portable equivalent of APFS clonefile(2); GNU cp on Linux uses
// copy_file_range() under the hood, which is already fast on ext4/xfs/btrfs.
func cloneTree(srcRepo, destPath string) error {
	return cpFallback(srcRepo, destPath)
}
