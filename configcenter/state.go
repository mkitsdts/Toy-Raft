package configcenter

import (
	"encoding/json"
	"errors"
	"fmt"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"nexus/raft"
)

// Command op types replicated through the Raft log. The value of a command is
// a JSON-encoded payload specific to its op type. Every state change in the
// configuration center (configs, namespaces, roles and Raft nodes) is expressed
// as one of these commands so all nodes converge on the same state.
const (
	OpConfigPut       = "config.put"
	OpConfigDelete    = "config.delete"
	OpNamespaceCreate = "namespace.create"
	OpNamespaceDelete = "namespace.delete"
	OpRoleCreate      = "role.create"
	OpRoleUpdate      = "role.update"
	OpRoleDelete      = "role.delete"
	OpRoleRotate      = "role.rotate"
	OpNodeRegister    = "node.register"
	OpNodeDelete      = "node.delete"
)

type configPutPayload struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Value     string `json:"value"`
	Format    string `json:"format"`
}

type configDeletePayload struct {
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
}

type namespacePayload struct {
	Name string `json:"name"`
}

type roleCreatePayload struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Token       string   `json:"token"`
	Namespaces  []string `json:"namespaces"`
}

type roleUpdatePayload struct {
	ID          uint     `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Namespaces  []string `json:"namespaces"`
}

type roleDeletePayload struct {
	ID uint `json:"id"`
}

type roleRotatePayload struct {
	ID    uint   `json:"id"`
	Token string `json:"token"`
}

type nodeRegisterPayload struct {
	IP   string `json:"ip"`
	Port string `json:"port"`
}

type nodeDeletePayload struct {
	ID uint `json:"id"`
}

// newCommand JSON-encodes payload as a Raft command of the given op type.
// Non-deterministic values (role tokens in particular) are generated once by
// the leader and carried in the payload so every node applies the same value.
func newCommand(opType string, payload any) (raft.Command, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return raft.Command{}, fmt.Errorf("encode %s command: %w", opType, err)
	}
	return raft.Command{OpType: opType, Value: data}, nil
}

func decodePayload(value []byte, v any) error {
	if err := json.Unmarshal(value, v); err != nil {
		return fmt.Errorf("decode command payload: %w", err)
	}
	return nil
}

// ApplyCommand is the Raft state machine. It is invoked on every node once a
// command is committed, in log order. It must be deterministic, so it only
// writes through the store and never consults external state.
func (s *Store) ApplyCommand(cmd raft.Command) error {
	switch cmd.OpType {
	case OpConfigPut:
		var p configPutPayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		_, err := s.Put(p.Namespace, p.Key, p.Value, p.Format)
		return err
	case OpConfigDelete:
		var p configDeletePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		err := s.Delete(p.Namespace, p.Key)
		if errors.Is(err, ErrNotFound) {
			// Deleting an already-absent key is a no-op, not a failure: the
			// command was still committed.
			return nil
		}
		return err
	case OpNamespaceCreate:
		var p namespacePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyNamespaceCreate(p)
	case OpNamespaceDelete:
		var p namespacePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyNamespaceDelete(p)
	case OpRoleCreate:
		var p roleCreatePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyRoleCreate(p)
	case OpRoleUpdate:
		var p roleUpdatePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyRoleUpdate(p)
	case OpRoleDelete:
		var p roleDeletePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyRoleDelete(p)
	case OpRoleRotate:
		var p roleRotatePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyRoleRotate(p)
	case OpNodeRegister:
		var p nodeRegisterPayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyNodeRegister(p)
	case OpNodeDelete:
		var p nodeDeletePayload
		if err := decodePayload(cmd.Value, &p); err != nil {
			return err
		}
		return s.applyNodeDelete(p)
	default:
		return fmt.Errorf("unknown command op type %q", cmd.OpType)
	}
}

// ---- namespaces -------------------------------------------------------------

func (s *Store) applyNamespaceCreate(p namespacePayload) error {
	ns := Namespace{Name: p.Name}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&ns).Error
}

// applyNamespaceDelete removes the namespace together with all configurations
// inside it and any role grant that references it.
func (s *Store) applyNamespaceDelete(p namespacePayload) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("namespace = ?", p.Name).Delete(&Configuration{}).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM role_namespaces WHERE namespace_name = ?", p.Name).Error; err != nil {
			return err
		}
		return tx.Delete(&Namespace{Name: p.Name}).Error
	})
}

// ---- roles ------------------------------------------------------------------

func (s *Store) applyRoleCreate(p roleCreatePayload) error {
	role := Role{Name: p.Name, Description: p.Description, Token: p.Token}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&role).Error; err != nil {
			return err
		}
		return assignNamespaces(tx, &role, p.Namespaces)
	})
}

func (s *Store) applyRoleUpdate(p roleUpdatePayload) error {
	role, err := s.RoleByID(p.ID)
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Role{ID: p.ID}).Updates(map[string]any{
			"name":        p.Name,
			"description": p.Description,
		}).Error; err != nil {
			return err
		}
		return assignNamespaces(tx, role, p.Namespaces)
	})
}

func (s *Store) applyRoleDelete(p roleDeletePayload) error {
	role, err := s.RoleByID(p.ID)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(role).Association("Namespaces").Clear(); err != nil {
			return err
		}
		return tx.Delete(&Role{}, p.ID).Error
	})
}

func (s *Store) applyRoleRotate(p roleRotatePayload) error {
	return s.db.Model(&Role{ID: p.ID}).Update("token", p.Token).Error
}

// ---- raft nodes -------------------------------------------------------------

func (s *Store) applyNodeRegister(p nodeRegisterPayload) error {
	node := NodeConfig{IP: p.IP, Port: p.Port}
	return s.db.Clauses(clause.OnConflict{DoNothing: true}).Create(&node).Error
}

func (s *Store) applyNodeDelete(p nodeDeletePayload) error {
	return s.db.Delete(&NodeConfig{}, p.ID).Error
}
