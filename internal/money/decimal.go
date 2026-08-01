// Package money provides exact decimal presentation calculations. PostgreSQL
// NUMERIC remains authoritative for persisted quotations and settlement.
package money

import (
	"fmt"
	"math/big"
	"strconv"
)

// Product rounds half away from zero, matching PostgreSQL round(numeric,n).
func Product(scale int, values ...string) (string, error) {
	r := big.NewRat(1, 1)
	for _, v := range values {
		p, ok := new(big.Rat).SetString(v)
		if !ok {
			return "", fmt.Errorf("invalid decimal %q", v)
		}
		r.Mul(r, p)
	}
	sign := r.Sign()
	r.Abs(r)
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	n := new(big.Int).Mul(r.Num(), factor)
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(n, r.Denom(), rem)
	if new(big.Int).Mul(rem, big.NewInt(2)).Cmp(r.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if sign < 0 {
		q.Neg(q)
	}
	return new(big.Rat).SetFrac(q, factor).FloatString(scale), nil
}

func Charge(kwh float64, tariff, rate string) (string, string, error) {
	energy, err := Product(3, strconv.FormatFloat(kwh, 'f', -1, 64))
	if err != nil {
		return "", "", err
	}
	original, err := Product(2, energy, tariff)
	if err != nil {
		return "", "", err
	}
	converted, err := Product(2, original, rate)
	return original, converted, err
}

func Float(decimal string) float64 { v, _ := strconv.ParseFloat(decimal, 64); return v }

// EnergyBudget floors to a millikWh with a two-cent rounding cushion. Physics
// can use the resulting float; no money arithmetic uses a binary float.
func EnergyBudget(budget, tariff, rate string) float64 {
	b, ok := new(big.Rat).SetString(budget)
	if !ok {
		return 0
	}
	p, ok := new(big.Rat).SetString(tariff)
	if !ok || p.Sign() <= 0 {
		return 0
	}
	r, ok := new(big.Rat).SetString(rate)
	if !ok || r.Sign() <= 0 {
		return 0
	}
	b.Sub(b, big.NewRat(2, 100))
	if b.Sign() <= 0 {
		return 0
	}
	b.Quo(b, new(big.Rat).Mul(p, r))
	b.Mul(b, big.NewRat(1000, 1))
	units := new(big.Int).Quo(b.Num(), b.Denom())
	value, _ := new(big.Rat).SetFrac(units, big.NewInt(1000)).Float64()
	return value
}
