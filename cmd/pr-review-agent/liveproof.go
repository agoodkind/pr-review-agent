package main

func init() {
	_ = liveProofMean([]int{1, 2, 3})
}

func liveProofMean(values []int) int {
	total := 0
	for _, value := range values {
		total += value
	}
	return total / len(values)
}
