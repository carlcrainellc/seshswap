package store

// syncDir: Windows can't flush a directory handle (access denied), and NTFS
// makes the rename durable with the file, so there is nothing to do.
func syncDir(string) error { return nil }
