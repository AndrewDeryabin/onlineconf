package resolver

import (
	"context"
	"database/sql"
	"sync"

	"github.com/rs/zerolog/log"
	. "gitlab.corp.mail.ru/mydev/onlineconf/admin/go/common"
)

type Param struct {
	ID           int
	Name         string
	Path         string
	MTime        string
	Version      int
	ContentType  string
	Value        NullString
	Children     map[string]**Param
	common       bool
	deepResolved bool
	seen         bool
	serialized   []byte
}

func (node *Param) deepMarkCommon(ctx context.Context) {
	ok := true
	for _, childPtr := range node.Children {
		child := *childPtr
		child.deepMarkCommon(ctx)
		if !child.common {
			ok = false
		}
	}
	switch node.ContentType {
	case "application/x-symlink", "application/x-case", "application/x-template":
		ok = false
	}
	node.common = ok
	node.deepResolved = ok
	if ok {
		if err := serializeParam(node); err != nil {
			log.Ctx(ctx).Warn().Str("param", node.Path).Msg("failed to serialize common param")
		}
	}
}

func (node *Param) deepClone() *Param {
	if node.deepResolved {
		return node
	}
	copy := *node
	copy.Children = make(map[string]**Param)
	for name, childPtr := range node.Children {
		childCopy := (*childPtr).deepClone()
		copy.Children[name] = &childCopy
	}
	return &copy
}

type tree struct {
	root        *Param
	mtime       string
	rw          sync.RWMutex
	datacenters []datacenter
	groups      []group
	services    services
}

func getTreeMTime(ctx context.Context) (string, error) {
	query := "SELECT max(MTime) FROM my_config_tree_log"
	row := DB.QueryRowContext(ctx, query)
	var mtime string
	err := row.Scan(&mtime)
	return mtime, err
}

func selectTree(ctx context.Context) (*Param, error) {
	query := `
		SELECT ID, Name, Path, MTime, Version, ContentType, Value, ParentID
		FROM my_config_tree
		WHERE NOT Deleted
		ORDER BY Path
	`
	rows, err := DB.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	paramByID := make(map[int]*Param)
	var root *Param
	for rows.Next() {
		param := Param{}
		param.Children = make(map[string]**Param)
		var parentID sql.NullInt64
		err = rows.Scan(&param.ID, &param.Name, &param.Path, &param.MTime, &param.Version, &param.ContentType, &param.Value, &parentID)
		if err != nil {
			return nil, nil
		}
		paramByID[param.ID] = &param
		if parentID.Valid {
			parent, ok := paramByID[int(parentID.Int64)]
			if !ok {
				panic("Parent not found")
			}
			paramPtr := &param
			parent.Children[param.Name] = &paramPtr
		} else {
			if root != nil {
				panic("Duplicate root detected")
			}
			root = &param
		}
	}
	root.deepMarkCommon(ctx)
	return root, nil
}

func (t *tree) update(ctx context.Context) error {
	isReadyToUpdate, mtime, err := t.readyToUpdate(ctx)
	if err != nil {
		return err
	}
	if !isReadyToUpdate {
		return nil
	}
	log.Ctx(ctx).Info().Str("mtime", mtime).Msg("updating graph")

	var root *Param
	var datacenters []datacenter
	var groups []group
	var services services

	root, err = selectTree(ctx)
	if err != nil {
		return err
	}

	cg := newCommonGraph(root.deepClone())
	if datacenters, err = cg.readDatacenters(ctx); err != nil {
		return err
	}
	if groups, err = cg.readGroups(ctx); err != nil {
		return err
	}
	if services, err = cg.readServices(ctx); err != nil {
		return err
	}

	t.rw.Lock()
	t.root = root
	t.datacenters = datacenters
	t.groups = groups
	t.services = services
	t.mtime = mtime
	t.rw.Unlock()
	return nil
}

func (t *tree) readyToUpdate(ctx context.Context) (res bool, mtime string, err error) {
	if mtime, err = getTreeMTime(ctx); err != nil {
		return false, mtime, err
	}
	t.rw.RLock()
	res = mtime > t.mtime
	t.rw.RUnlock()
	return
}

func (t *tree) getServicePwdHash(username string) (pwdHash []byte, exists bool) {
	t.rw.RLock()
	pwdHash, exists = t.services[username]
	t.rw.RUnlock()
	return
}
