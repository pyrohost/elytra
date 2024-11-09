package filesystem

import (
	"bytes"
	"strconv"
)

func isIgnoredInCurseForgeFingerprint(b byte) bool {
	return b == '\t' || b == '\n' || b == '\r' || b == ' '
}

func computeCurseForgeFingerprintNormalizedLength(buf *bytes.Buffer) int {
	var len_no_whitespace int = 0
	bytes := buf.Bytes()

	for i := 0; i < buf.Len(); i++ {
		char := bytes[i]
		if !isIgnoredInCurseForgeFingerprint(char) {
			len_no_whitespace++
		}
	}

	return len_no_whitespace
}

// https://github.com/meza/curseforge-fingerprint/blob/main/src/addon/fingerprint.cpp#L36
func CalculateCurseForgeFingerprint(buf *bytes.Buffer) string {
	const multiplex = 1540483477
	len := buf.Len()
	bytes := buf.Bytes()

	var num1 uint32 = uint32(computeCurseForgeFingerprintNormalizedLength(buf))
	var num2 uint32 = 1 ^ num1

	var num3 uint32 = 0
	var num4 uint32 = 0

	for i := 0; i < len; i++ {
		b := bytes[i]

		if !isIgnoredInCurseForgeFingerprint(b) {
			num3 |= uint32(b) << num4
			num4 += 8

			if num4 == 32 {
				var num6 uint32 = num3 * multiplex
				var num7 uint32 = (num6 ^ num6>>24) * multiplex

				num2 = num2*multiplex ^ num7
				num3 = 0
				num4 = 0
			}
		}
	}

	if num4 > 0 {
		num2 = (num2 ^ num3) * multiplex
	}

	num6 := (num2 ^ num2>>13) * multiplex

	return strconv.FormatUint(uint64(num6^num6>>15), 10)
}
