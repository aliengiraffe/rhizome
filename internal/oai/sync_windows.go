package oai

// Go's file Sync flushes the temporary file before its same-directory rename.
// Windows does not support syncing an opened directory with os.File.Sync.
func syncStateDir(string) error { return nil }
