package private

import "runtime"

func DefaultParallelism() int {
	return max(1, runtime.NumCPU()-2)
}

func (m Manager) concurrency(nodes int) int {
	if m.Parallelism == nil {
		return boundedConcurrency(nodes, DefaultParallelism())
	}
	if *m.Parallelism == 0 {
		return max(1, nodes)
	}
	return boundedConcurrency(nodes, *m.Parallelism)
}

func boundedConcurrency(nodes, parallelism int) int {
	if parallelism <= 0 {
		parallelism = DefaultParallelism()
	}
	return min(max(1, nodes), parallelism)
}
