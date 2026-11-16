package kyc

import "net/http"

// ValidateNIK checks the basic structure of a 16-digit NIK: province code 11–94, birth day 01–31 (or
// 41–71 for women), month 01–12, and a non-zero sequence number.
func ValidateNIK(nik string) error {
	if len(nik) != 16 {
		return errNIKLength
	}
	for _, r := range nik {
		if r < '0' || r > '9' {
			return errNIKLength
		}
	}
	two := func(i int) int { return int(nik[i]-'0')*10 + int(nik[i+1]-'0') }
	prov, day, month := two(0), two(6), two(8)
	if prov < 11 || prov > 94 {
		return errNIKFormat
	}
	if day > 40 {
		day -= 40
	}
	if day < 1 || day > 31 || month < 1 || month > 12 || nik[12:] == "0000" {
		return errNIKFormat
	}
	return nil
}

// MaskNIK returns the first 4 and last 4 digits with the middle masked: 3404********0001.
func MaskNIK(nik string) string {
	if len(nik) != 16 {
		return "****************"
	}
	return nik[:4] + "********" + nik[12:]
}

var (
	errNIKLength = userErr(http.StatusBadRequest, "NIK harus 16 digit angka.")
	errNIKFormat = userErr(http.StatusBadRequest, "Format NIK tidak valid. Cek lagi angka di KTP-mu.")
)
