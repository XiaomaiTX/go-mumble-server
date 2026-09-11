package database

import (
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/dchote/go-mumble-server/internal/database/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Open opens a SQLite connection and runs migrations.
func Open(path string) (*gorm.DB, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, err
	}

	// Probe writability up front so a permissions problem on the data
	// directory surfaces as an explicit message instead of SQLite's
	// cryptic "unable to open database file".
	probe, err := os.CreateTemp(dir, ".dbprobe-*")
	if err != nil {
		return nil, fmt.Errorf("data directory %s is not writable by uid %d: %w", dir, os.Getuid(), err)
	}
	probe.Close()
	os.Remove(probe.Name())

	gormLogger := logger.New(
		log.New(os.Stdout, "\r\n", log.LstdFlags),
		logger.Config{
			IgnoreRecordNotFoundError: true,
		},
	)

	db, err := gorm.Open(sqlite.Open(path), &gorm.Config{Logger: gormLogger})
	if err != nil {
		return nil, err
	}

	if err := db.AutoMigrate(
		&models.User{},
		&models.VirtualServer{},
		&models.Channel{},
		&models.Ban{},
		&models.ServerConfig{},
		&models.MetaConfig{},
		&models.ChannelGroup{},
		&models.ChannelACL{},
		&models.RegisteredUser{},
	); err != nil {
		return nil, err
	}

	return db, nil
}
