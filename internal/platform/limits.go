package platform

// Limits caps the resources of an engine run and everything it starts.
// Zero means unlimited.
type Limits struct {
	MemoryMB     int // total memory of the process tree
	MaxProcesses int // concurrently running processes in the tree (Windows)
}
