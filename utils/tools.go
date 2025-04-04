package utils

func InArrayInt(a int, arr []int) bool {
	for _, v := range arr {
		if v == a {
			return true
		}
	}
	return false
}
