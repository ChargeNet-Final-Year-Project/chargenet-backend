package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Vehicle is the public vehicle DTO.
type Vehicle struct {
	ID              string  `json:"id"`
	UserID          string  `json:"userId"`
	Name            string  `json:"name"`
	LicensePlate    string  `json:"licensePlate"`
	ConnectorType   string  `json:"connectorType"`
	BatteryCapacity float64 `json:"batteryCapacity"`
	IsDefault       bool    `json:"isDefault"`
}

// VehicleInput is the (already validated) write payload for create/update.
// id and userId are always derived from the session, never the body.
type VehicleInput struct {
	Name            string
	LicensePlate    string
	ConnectorType   string
	BatteryCapacity float64
	IsDefault       bool
}

// vehicleColumns is the shared SELECT/RETURNING projection.
const vehicleColumns = `id, user_id, name, license_plate, connector_type, battery_capacity, is_default`

// ListVehicles returns the driver's vehicles with the default one first.
func (s *Store) ListVehicles(ctx context.Context, userID string) ([]Vehicle, error) {
	rows, err := s.DB.QueryContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicles WHERE user_id = $1
		  ORDER BY is_default DESC, created_at ASC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Vehicle{}
	for rows.Next() {
		var v Vehicle
		if err := rows.Scan(&v.ID, &v.UserID, &v.Name, &v.LicensePlate,
			&v.ConnectorType, &v.BatteryCapacity, &v.IsDefault); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// DefaultVehicle returns the driver's default vehicle, or (nil, nil) when the
// driver has no vehicle on file — starting a charge without one is allowed.
func (s *Store) DefaultVehicle(ctx context.Context, userID string) (*Vehicle, error) {
	v, err := scanVehicle(s.DB.QueryRowContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicles WHERE user_id = $1
		  ORDER BY is_default DESC, created_at ASC, id ASC LIMIT 1`, userID))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	return v, err
}

// GetVehicle loads one vehicle, scoped to its owner (other users' ids are
// indistinguishable from missing ones: ErrNotFound).
func (s *Store) GetVehicle(ctx context.Context, userID, id string) (*Vehicle, error) {
	return scanVehicle(s.DB.QueryRowContext(ctx,
		`SELECT `+vehicleColumns+` FROM vehicles WHERE id = $1 AND user_id = $2`, id, userID))
}

// CreateVehicle inserts a vehicle. The first vehicle of a driver is always the
// default; requesting isDefault unsets any previous default in the same
// transaction (the partial unique index would otherwise reject the insert).
func (s *Store) CreateVehicle(ctx context.Context, userID string, in VehicleInput) (*Vehicle, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var hasAny bool
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM vehicles WHERE user_id = $1)`, userID).Scan(&hasAny); err != nil {
		return nil, err
	}
	makeDefault := in.IsDefault || !hasAny

	if makeDefault {
		if _, err := tx.ExecContext(ctx,
			`UPDATE vehicles SET is_default = FALSE WHERE user_id = $1 AND is_default`, userID); err != nil {
			return nil, err
		}
	}

	v, err := scanVehicle(tx.QueryRowContext(ctx,
		`INSERT INTO vehicles (id, user_id, name, license_plate, connector_type, battery_capacity, is_default)
		 VALUES ($1, $2, $3, $4, $5, $6, $7)
		 RETURNING `+vehicleColumns,
		NewID("veh"), userID, in.Name, in.LicensePlate, in.ConnectorType, in.BatteryCapacity, makeDefault))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

// UpdateVehicle replaces a vehicle's editable fields. Only the owner can do
// this (another driver's id is a 404). Setting isDefault unsets the previous
// default in the same transaction; clearing the flag on the current default is
// ignored so a driver with vehicles always keeps exactly one.
func (s *Store) UpdateVehicle(ctx context.Context, userID, id string, in VehicleInput) (*Vehicle, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var wasDefault bool
	err = tx.QueryRowContext(ctx,
		`SELECT is_default FROM vehicles WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, userID).Scan(&wasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}

	makeDefault := in.IsDefault || wasDefault
	if makeDefault && !wasDefault {
		if _, err := tx.ExecContext(ctx,
			`UPDATE vehicles SET is_default = FALSE WHERE user_id = $1 AND is_default`, userID); err != nil {
			return nil, err
		}
	}

	v, err := scanVehicle(tx.QueryRowContext(ctx,
		`UPDATE vehicles
		    SET name = $3, license_plate = $4, connector_type = $5, battery_capacity = $6, is_default = $7
		  WHERE id = $1 AND user_id = $2
		  RETURNING `+vehicleColumns,
		id, userID, in.Name, in.LicensePlate, in.ConnectorType, in.BatteryCapacity, makeDefault))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

// DeleteVehicle removes a vehicle. When the deleted row was the default, the
// oldest remaining vehicle is promoted in the same transaction.
func (s *Store) DeleteVehicle(ctx context.Context, userID, id string) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	var wasDefault bool
	err = tx.QueryRowContext(ctx,
		`DELETE FROM vehicles WHERE id = $1 AND user_id = $2 RETURNING is_default`, id, userID).Scan(&wasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}

	if wasDefault {
		if _, err := tx.ExecContext(ctx,
			`UPDATE vehicles SET is_default = TRUE
			  WHERE id = (SELECT id FROM vehicles WHERE user_id = $1 ORDER BY created_at ASC, id ASC LIMIT 1)`,
			userID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func scanVehicle(row *sql.Row) (*Vehicle, error) {
	v := &Vehicle{}
	err := row.Scan(&v.ID, &v.UserID, &v.Name, &v.LicensePlate,
		&v.ConnectorType, &v.BatteryCapacity, &v.IsDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return v, nil
}

// NormalizeLicensePlate trims and upper-cases a plate before it is stored.
func NormalizeLicensePlate(plate string) string {
	return strings.ToUpper(strings.TrimSpace(plate))
}
