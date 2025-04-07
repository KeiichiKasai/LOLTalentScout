package utils

func InArrayInt(a int, arr []int) bool {
	for _, v := range arr {
		if v == a {
			return true
		}
	}
	return false
}

func TruncateString(s string, maxRunes int) string {
	runes := []rune(s)
	if len(runes) > maxRunes {
		return string(runes[:maxRunes])
	}
	return s
}
