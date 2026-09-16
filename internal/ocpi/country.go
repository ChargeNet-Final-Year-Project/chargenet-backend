package ocpi

import "strings"

func CountryAlpha2(country string) string {
	switch strings.ToUpper(country) {
	case "MYS", "MY":
		return "MY"
	case "SGP", "SG":
		return "SG"
	case "THA", "TH":
		return "TH"
	}
	return ""
}

func CountryAlpha3(country string) string {
	switch CountryAlpha2(country) {
	case "MY":
		return "MYS"
	case "SG":
		return "SGP"
	case "TH":
		return "THA"
	}
	return ""
}

func CountryTimeZone(country string) string {
	switch CountryAlpha2(country) {
	case "SG":
		return "Asia/Singapore"
	case "TH":
		return "Asia/Bangkok"
	}
	return "Asia/Kuala_Lumpur"
}
