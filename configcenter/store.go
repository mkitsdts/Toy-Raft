package configcenter

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var ErrNotFound = errors.New("record not found")

type Store struct {
	db *gorm.DB
}

func OpenStore(cfg DatabaseConfig) (*Store, error) {
	var dialector gorm.Dialector
	switch cfg.Driver {
	case "sqlite", "sqlite3":
		dialector = sqlite.Open(cfg.DSN)
	default:
		return nil, fmt.Errorf("unsupported database driver %q (currently supported: sqlite3)", cfg.Driver)
	}

	db, err := gorm.Open(dialector, &gorm.Config{})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.AutoMigrate(&Configuration{}, &SystemSetting{}, &Namespace{}, &Role{}); err != nil {
		return nil, fmt.Errorf("migrate database: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the underlying database connection.
func (s *Store) Close() error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.Close()
}

// ---- configurations ---------------------------------------------------------

// ErrInvalidFormat marks a configuration value that does not parse as its
// declared format.
var ErrInvalidFormat = errors.New("configuration value does not match its format")

// ValidateFormat checks that value parses as format. format must be one of
// FormatText, FormatJSON or FormatYAML; an empty format defaults to text.
func ValidateFormat(format, value string) error {
	switch format {
	case "", FormatText:
		return nil
	case FormatJSON:
		var v any
		if err := json.Unmarshal([]byte(value), &v); err != nil {
			return fmt.Errorf("%w: invalid json: %v", ErrInvalidFormat, err)
		}
		return nil
	case FormatYAML:
		var v any
		if err := yaml.Unmarshal([]byte(value), &v); err != nil {
			return fmt.Errorf("%w: invalid yaml: %v", ErrInvalidFormat, err)
		}
		return nil
	default:
		return fmt.Errorf("%w: unsupported format %q (use text, json or yaml)", ErrInvalidFormat, format)
	}
}

// Put creates or updates a configuration. An empty format defaults to text.
func (s *Store) Put(namespace, key, value, format string) (*Configuration, error) {
	if format == "" {
		format = FormatText
	}
	if err := ValidateFormat(format, value); err != nil {
		return nil, err
	}
	var result Configuration
	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := ensureNamespace(tx, namespace); err != nil {
			return err
		}
		err := tx.Where("namespace = ? AND key = ?", namespace, key).First(&result).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			result = Configuration{Namespace: namespace, Key: key, Value: value, Format: format, Version: 1}
			return tx.Create(&result).Error
		}
		if err != nil {
			return err
		}
		result.Value = value
		result.Format = format
		result.Version++
		return tx.Save(&result).Error
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func (s *Store) Get(namespace, key string) (*Configuration, error) {
	var result Configuration
	err := s.db.Where("namespace = ? AND key = ?", namespace, key).First(&result).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &result, nil
}

// ListConfigs returns configurations ordered by namespace and key. A nil
// namespaces slice means "every namespace"; an empty slice means none.
func (s *Store) ListConfigs(namespaces []string) ([]Configuration, error) {
	configs := []Configuration{}
	query := s.db.Order("namespace asc, key asc")
	if namespaces != nil {
		if len(namespaces) == 0 {
			return configs, nil
		}
		query = query.Where("namespace IN ?", namespaces)
	}
	if err := query.Find(&configs).Error; err != nil {
		return nil, err
	}
	return configs, nil
}

func (s *Store) Delete(namespace, key string) error {
	result := s.db.Where("namespace = ? AND key = ?", namespace, key).Delete(&Configuration{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- namespaces -------------------------------------------------------------

func (s *Store) ListNamespaces() ([]string, error) {
	names := []string{}
	if err := s.db.Model(&Namespace{}).Order("name asc").Pluck("name", &names).Error; err != nil {
		return nil, err
	}
	return names, nil
}

func ensureNamespace(tx *gorm.DB, name string) error {
	ns := Namespace{Name: name}
	return tx.Where("name = ?", name).FirstOrCreate(&ns).Error
}

// ---- roles ------------------------------------------------------------------

func (s *Store) ListRoles() ([]Role, error) {
	roles := []Role{}
	if err := s.db.Preload("Namespaces").Order("id asc").Find(&roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

func (s *Store) RoleByID(id uint) (*Role, error) {
	var role Role
	err := s.db.Preload("Namespaces").First(&role, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &role, nil
}

func (s *Store) RoleByToken(token string) (*Role, error) {
	var role Role
	err := s.db.Preload("Namespaces").Where("token = ?", token).First(&role).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &role, nil
}

func (s *Store) CreateRole(name, description string, namespaces []string) (*Role, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	role := Role{Name: name, Description: description, Token: token}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
		return assignNamespaces(tx, &role, namespaces)
	})
	if err != nil {
		return nil, err
	}
	return s.RoleByID(role.ID)
}

func (s *Store) UpdateRole(id uint, name, description string, namespaces []string) (*Role, error) {
	role, err := s.RoleByID(id)
	if err != nil {
		return nil, err
	}
	err = s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Role{ID: id}).Updates(map[string]any{
			"name":        name,
			"description": description,
		}).Error; err != nil {
			return err
		}
		return assignNamespaces(tx, role, namespaces)
	})
	if err != nil {
		return nil, err
	}
	return s.RoleByID(id)
}

func (s *Store) RotateRoleToken(id uint) (*Role, error) {
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	result := s.db.Model(&Role{ID: id}).Update("token", token)
	if result.Error != nil {
		return nil, result.Error
	}
	if result.RowsAffected == 0 {
		return nil, ErrNotFound
	}
	return s.RoleByID(id)
}

func (s *Store) DeleteRole(id uint) error {
	role, err := s.RoleByID(id)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(role).Association("Namespaces").Clear(); err != nil {
			return err
		}
		return tx.Delete(&Role{}, id).Error
	})
}

func assignNamespaces(tx *gorm.DB, role *Role, namespaces []string) error {
	objects := make([]Namespace, 0, len(namespaces))
	for _, name := range namespaces {
		ns := Namespace{Name: name}
		if err := tx.Where("name = ?", name).FirstOrCreate(&ns).Error; err != nil {
			return err
		}
		objects = append(objects, ns)
	}
	return tx.Model(role).Association("Namespaces").Replace(objects)
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "cc_" + hex.EncodeToString(buf), nil
}

// newAdminToken returns a short random admin token built from the leading part
// of a UUID v4 (e.g. "a3f1c9d2"). It is regenerated on every start so the value
// is never fixed.
func newAdminToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	return uuid[:8], nil
}

// IsDuplicateError reports whether err is a unique-constraint violation.
func IsDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "UNIQUE constraint failed")
}
