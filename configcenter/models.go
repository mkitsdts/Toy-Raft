package configcenter

import "time"

// Supported configuration value formats.
const (
	FormatText = "text"
	FormatJSON = "json"
	FormatYAML = "yaml"
)

// Configuration is one versioned configuration item. Namespace and key are
// unique, so registering an existing item updates it atomically. Format
// declares how Value should be parsed: text, json or yaml.
type Configuration struct {
	ID        uint      `json:"-" gorm:"primaryKey"`
	Namespace string    `json:"namespace" gorm:"size:255;not null;uniqueIndex:idx_namespace_key"`
	Key       string    `json:"key" gorm:"size:255;not null;uniqueIndex:idx_namespace_key"`
	Value     string    `json:"value" gorm:"type:text;not null"`
	Format    string    `json:"format" gorm:"size:16;not null;default:text"`
	Version   uint64    `json:"version" gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SystemSetting contains node and authentication settings persisted in the
// database. There is exactly one row, identified by ID=1.
type SystemSetting struct {
	ID        uint   `gorm:"primaryKey"`
	LocalIP   string `gorm:"size:255;not null"`
	NodeIPs   string `gorm:"type:text;not null"`
	AuthToken string `gorm:"size:512;not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Namespace groups configurations and is the unit a role is granted access to.
// It exists as its own table so roles can reference namespaces that do not have
// configurations yet.
type Namespace struct {
	Name      string    `json:"name" gorm:"primaryKey;size:255"`
	CreatedAt time.Time `json:"created_at"`
}

// Role binds a token to a set of namespaces. Presenting the token returns every
// configuration inside those namespaces.
type Role struct {
	ID          uint        `json:"id" gorm:"primaryKey"`
	Name        string      `json:"name" gorm:"size:255;uniqueIndex;not null"`
	Description string      `json:"description" gorm:"size:512"`
	Token       string      `json:"token" gorm:"size:512;uniqueIndex;not null"`
	Namespaces  []Namespace `json:"namespaces" gorm:"many2many:role_namespaces;"`
	CreatedAt   time.Time   `json:"created_at"`
	UpdatedAt   time.Time   `json:"updated_at"`
}

// NamespaceList returns the role's namespace names.
func (r *Role) NamespaceList() []string {
	names := make([]string, 0, len(r.Namespaces))
	for _, ns := range r.Namespaces {
		names = append(names, ns.Name)
	}
	return names
}
