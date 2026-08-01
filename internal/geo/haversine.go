// Package geo holds small geographic helpers used by the station search.
package geo

import "math"

// earthRadiusKM is the mean Earth radius used by the haversine formula.
const earthRadiusKM = 6371.0

// HaversineKM returns the great-circle distance in kilometres between two
// points given in decimal degrees.
func HaversineKM(lat1, lon1, lat2, lon2 float64) float64 {
	const toRad = math.Pi / 180

	dLat := (lat2 - lat1) * toRad
	dLon := (lon2 - lon1) * toRad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*toRad)*math.Cos(lat2*toRad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusKM * c
}

// Round1 rounds to one decimal place (the precision the API contracts on).
func Round1(v float64) float64 {
	return math.Round(v*10) / 10
}
