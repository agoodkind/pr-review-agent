// Package liveproof supplies temporary production review fixtures.
package liveproof

func init() {
	_ = Mean([]int{1, 2})
}

// Mean returns the integer average of the supplied values.
func Mean(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total / len(values)
}
