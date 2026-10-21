package channel

// sourceOpenFlags adds nothing on Windows, which has no O_NOFOLLOW; os.Root
// confinement and the Lstat/SameFile checks still refuse swapped sources.
const sourceOpenFlags = 0
