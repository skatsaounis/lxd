//go:build linux && cgo && !agent

package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/lxc/lxd/shared/api"
)

// CreateNetworkLoadBalancer creates a new Network Load Balancer.
// If memberSpecific is true, then the load balancer is associated to the current member, rather than being
// associated to all members.
func (c *Cluster) CreateNetworkLoadBalancer(networkID int64, memberSpecific bool, info *api.NetworkLoadBalancersPost) (int64, error) {
	var err error
	var loadBalancerID int64
	var nodeID any

	if memberSpecific {
		nodeID = c.nodeID
	}

	var backendsJSON, portsJSON []byte

	if info.Backends != nil {
		backendsJSON, err = json.Marshal(info.Backends)
		if err != nil {
			return -1, fmt.Errorf("Failed marshalling backends: %w", err)
		}
	}

	if info.Ports != nil {
		portsJSON, err = json.Marshal(info.Ports)
		if err != nil {
			return -1, fmt.Errorf("Failed marshalling ports: %w", err)
		}
	}

	err = c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		// Insert a new Network Load Balancer record.
		result, err := tx.tx.Exec(`
		INSERT INTO networks_load_balancers
		(network_id, node_id, listen_address, description, backends, ports)
		VALUES (?, ?, ?, ?, ?, ?)
		`, networkID, nodeID, info.ListenAddress, info.Description, string(backendsJSON), string(portsJSON))
		if err != nil {
			return err
		}

		loadBalancerID, err = result.LastInsertId()
		if err != nil {
			return err
		}

		// Save config.
		err = networkLoadBalancerConfigAdd(tx.tx, loadBalancerID, info.Config)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return -1, err
	}

	return loadBalancerID, err
}

// networkLoadBalancerConfigAdd inserts Network Load Balancer config keys.
func networkLoadBalancerConfigAdd(tx *sql.Tx, loadBalancerID int64, config map[string]string) error {
	stmt, err := tx.Prepare(`
	INSERT INTO networks_load_balancers_config
	(network_load_balancer_id, key, value)
	VALUES(?, ?, ?)
	`)
	if err != nil {
		return err
	}

	defer func() { _ = stmt.Close() }()

	for k, v := range config {
		if v == "" {
			continue
		}

		_, err = stmt.Exec(loadBalancerID, k, v)
		if err != nil {
			return fmt.Errorf("Failed inserting config: %w", err)
		}
	}

	return nil
}

// UpdateNetworkLoadBalancer updates an existing Network Load Balancer.
func (c *Cluster) UpdateNetworkLoadBalancer(networkID int64, loadBalancerID int64, info *api.NetworkLoadBalancerPut) error {
	var err error
	var backendsJSON, portsJSON []byte

	if info.Backends != nil {
		backendsJSON, err = json.Marshal(info.Backends)
		if err != nil {
			return fmt.Errorf("Failed marshalling backends: %w", err)
		}
	}

	if info.Ports != nil {
		portsJSON, err = json.Marshal(info.Ports)
		if err != nil {
			return fmt.Errorf("Failed marshalling ports: %w", err)
		}
	}

	err = c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		// Update existing Network Load Balancer record.
		res, err := tx.tx.Exec(`
		UPDATE networks_load_balancers
		SET description = ?, backends = ?, ports = ?
		WHERE network_id = ? and id = ?
		`, info.Description, string(backendsJSON), string(portsJSON), networkID, loadBalancerID)
		if err != nil {
			return err
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return err
		}

		if rowsAffected <= 0 {
			return api.StatusErrorf(http.StatusNotFound, "Network load balancer not found")
		}

		// Save config.
		_, err = tx.tx.Exec("DELETE FROM networks_load_balancers_config WHERE network_load_balancer_id=?", loadBalancerID)
		if err != nil {
			return err
		}

		err = networkLoadBalancerConfigAdd(tx.tx, loadBalancerID, info.Config)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// DeleteNetworkLoadBalancer deletes an existing Network Load Balancer.
func (c *Cluster) DeleteNetworkLoadBalancer(networkID int64, loadBalancerID int64) error {
	return c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		// Delete existing Network Load Balancer record.
		res, err := tx.tx.Exec(`
			DELETE FROM networks_load_balancers
			WHERE network_id = ? and id = ?
		`, networkID, loadBalancerID)
		if err != nil {
			return err
		}

		rowsAffected, err := res.RowsAffected()
		if err != nil {
			return err
		}

		if rowsAffected <= 0 {
			return api.StatusErrorf(http.StatusNotFound, "Network load balancer not found")
		}

		return nil
	})
}

// GetNetworkLoadBalancer returns the Network Load Balancer ID and info for the given network ID and listen address.
// If memberSpecific is true, then the search is restricted to load balancers that belong to this member or belong
// to all members.
func (c *Cluster) GetNetworkLoadBalancer(networkID int64, memberSpecific bool, listenAddress string) (int64, *api.NetworkLoadBalancer, error) {
	var q *strings.Builder = &strings.Builder{}
	args := []any{networkID, listenAddress}

	q.WriteString(`
	SELECT
		IFNULL(networks_load_balancers.id, -1) ,
		IFNULL(networks_load_balancers.listen_address, ""),
		IFNULL(networks_load_balancers.description, ""),
		IFNULL(nodes.name, "") as location,
		IFNULL(networks_load_balancers.backends, ""),
		IFNULL(networks_load_balancers.ports, ""),
		COUNT(networks_load_balancers.id) as rowCount
	FROM networks_load_balancers
	LEFT JOIN nodes ON nodes.id = networks_load_balancers.node_id
	WHERE networks_load_balancers.network_id = ? AND networks_load_balancers.listen_address = ?
	`)

	if memberSpecific {
		q.WriteString("AND (networks_load_balancers.node_id = ? OR networks_load_balancers.node_id IS NULL) ")
		args = append(args, c.nodeID)
	}

	var err error
	var loadBalancerID int64 = int64(-1)
	var loadBalancer api.NetworkLoadBalancer
	var backendsJSON, portsJSON string

	err = c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		var rowCount int

		err = tx.tx.QueryRow(q.String(), args...).Scan(&loadBalancerID, &loadBalancer.ListenAddress, &loadBalancer.Description, &loadBalancer.Location, &backendsJSON, &portsJSON, &rowCount)
		if (err == nil && rowCount <= 0) || errors.Is(err, sql.ErrNoRows) {
			return api.StatusErrorf(http.StatusNotFound, "Network load balancer not found")
		} else if err == nil && rowCount > 1 {
			return api.StatusErrorf(http.StatusConflict, "Network load balancer found on more than one cluster member. Please target a specific member")
		} else if err != nil {
			return err
		}

		err = networkLoadBalancerConfig(tx, loadBalancerID, &loadBalancer)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return -1, nil, err
	}

	loadBalancer.Ports = []api.NetworkLoadBalancerPort{}
	if portsJSON != "" {
		err = json.Unmarshal([]byte(portsJSON), &loadBalancer.Ports)
		if err != nil {
			return -1, nil, fmt.Errorf("Failed unmarshalling ports: %w", err)
		}
	}

	loadBalancer.Backends = []api.NetworkLoadBalancerBackend{}
	if backendsJSON != "" {
		err = json.Unmarshal([]byte(backendsJSON), &loadBalancer.Backends)
		if err != nil {
			return -1, nil, fmt.Errorf("Failed unmarshalling backends: %w", err)
		}
	}

	return loadBalancerID, &loadBalancer, nil
}

// networkLoadBalancerConfig populates the config map of the Network Load Balancer with the given ID.
func networkLoadBalancerConfig(tx *ClusterTx, loadBalancerID int64, loadBalancer *api.NetworkLoadBalancer) error {
	q := `
	SELECT
		key,
		value
	FROM networks_load_balancers_config
	WHERE network_load_balancer_id=?
	`

	loadBalancer.Config = make(map[string]string)
	return tx.QueryScan(q, func(scan func(dest ...any) error) error {
		var key, value string

		err := scan(&key, &value)
		if err != nil {
			return err
		}

		_, found := loadBalancer.Config[key]
		if found {
			return fmt.Errorf("Duplicate config row found for key %q for network load balancer ID %d", key, loadBalancerID)
		}

		loadBalancer.Config[key] = value

		return nil
	}, loadBalancerID)
}

// GetNetworkLoadBalancerListenAddresses returns map of Network Load Balancer Listen Addresses for the given
// network ID keyed on Load Balancer ID.
// If memberSpecific is true, then the search is restricted to load balancers that belong to this member or belong
// to all members.
func (c *Cluster) GetNetworkLoadBalancerListenAddresses(networkID int64, memberSpecific bool) (map[int64]string, error) {
	var q *strings.Builder = &strings.Builder{}
	args := []any{networkID}

	q.WriteString(`
	SELECT
		id,
		listen_address
	FROM networks_load_balancers
	WHERE networks_load_balancers.network_id = ?
	`)

	if memberSpecific {
		q.WriteString("AND (networks_load_balancers.node_id = ? OR networks_load_balancers.node_id IS NULL) ")
		args = append(args, c.nodeID)
	}

	loadBalancers := make(map[int64]string)

	err := c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		return tx.QueryScan(q.String(), func(scan func(dest ...any) error) error {
			var loadBalancerID int64 = int64(-1)
			var listenAddress string

			err := scan(&loadBalancerID, &listenAddress)
			if err != nil {
				return err
			}

			loadBalancers[loadBalancerID] = listenAddress

			return nil
		}, args...)
	})
	if err != nil {
		return nil, err
	}

	return loadBalancers, nil
}

// GetProjectNetworkLoadBalancerListenAddressesByUplink returns map of Network Load Balancer Listen Addresses
// that belong to networks connected to the specified uplinkNetworkName.
// Returns a map keyed on project name and network ID containing a slice of listen addresses.
func (c *ClusterTx) GetProjectNetworkLoadBalancerListenAddressesByUplink(uplinkNetworkName string) (map[string]map[int64][]string, error) {
	// As uplink networks can only be in default project, it is safe to look for networks that reference the
	// specified uplinkNetworkName in their "network" config property.
	q := `
	SELECT
		projects.name,
		networks.id,
		networks_load_balancers.listen_address
	FROM networks_load_balancers
	JOIN networks on networks.id = networks_load_balancers.network_id
	JOIN networks_config on networks.id = networks_config.network_id
	JOIN projects ON projects.id = networks.project_id
	WHERE networks_config.key = "network"
	AND networks_config.value = ?
	`
	loadBalancers := make(map[string]map[int64][]string)

	err := c.QueryScan(q, func(scan func(dest ...any) error) error {
		var projectName string
		var networkID int64 = int64(-1)
		var listenAddress string

		err := scan(&projectName, &networkID, &listenAddress)
		if err != nil {
			return err
		}

		if loadBalancers[projectName] == nil {
			loadBalancers[projectName] = make(map[int64][]string)
		}

		if loadBalancers[projectName][networkID] == nil {
			loadBalancers[projectName][networkID] = make([]string, 0)
		}

		loadBalancers[projectName][networkID] = append(loadBalancers[projectName][networkID], listenAddress)

		return nil
	}, uplinkNetworkName)
	if err != nil {
		return nil, err
	}

	return loadBalancers, nil
}

// GetProjectNetworkLoadBalancerListenAddressesOnMember returns map of Network Load Balancer Listen Addresses that
// belong to to this specific cluster member. Will not include load balancers that do not have a specific member.
// Returns a map keyed on project name and network ID containing a slice of listen addresses.
func (c *ClusterTx) GetProjectNetworkLoadBalancerListenAddressesOnMember() (map[string]map[int64][]string, error) {
	q := `
	SELECT
		projects.name,
		networks.id,
		networks_load_balancers.listen_address
	FROM networks_load_balancers
	JOIN networks on networks.id = networks_load_balancers.network_id
	JOIN projects ON projects.id = networks.project_id
	WHERE networks_load_balancers.node_id = ?
	`
	loadBalancers := make(map[string]map[int64][]string)

	err := c.QueryScan(q, func(scan func(dest ...any) error) error {
		var projectName string
		var networkID int64 = int64(-1)
		var listenAddress string

		err := scan(&projectName, &networkID, &listenAddress)
		if err != nil {
			return err
		}

		if loadBalancers[projectName] == nil {
			loadBalancers[projectName] = make(map[int64][]string)
		}

		if loadBalancers[projectName][networkID] == nil {
			loadBalancers[projectName][networkID] = make([]string, 0)
		}

		loadBalancers[projectName][networkID] = append(loadBalancers[projectName][networkID], listenAddress)

		return nil
	}, c.nodeID)
	if err != nil {
		return nil, err
	}

	return loadBalancers, nil
}

// GetNetworkLoadBalancers returns map of Network Load Balancers for the given network ID keyed on Load Balancer ID.
// If memberSpecific is true, then the search is restricted to load balancers that belong to this member or belong
// to all members.
func (c *Cluster) GetNetworkLoadBalancers(networkID int64, memberSpecific bool) (map[int64]*api.NetworkLoadBalancer, error) {
	var q *strings.Builder = &strings.Builder{}
	args := []any{networkID}

	q.WriteString(`
	SELECT
		networks_load_balancers.id,
		networks_load_balancers.listen_address,
		networks_load_balancers.description,
		IFNULL(nodes.name, "") as location,
		networks_load_balancers.backends,
		networks_load_balancers.ports
	FROM networks_load_balancers
	LEFT JOIN nodes ON nodes.id = networks_load_balancers.node_id
	WHERE networks_load_balancers.network_id = ?
	`)

	if memberSpecific {
		q.WriteString("AND (networks_load_balancers.node_id = ? OR networks_load_balancers.node_id IS NULL) ")
		args = append(args, c.nodeID)
	}

	var err error
	loadBalancers := make(map[int64]*api.NetworkLoadBalancer)

	err = c.Transaction(context.TODO(), func(ctx context.Context, tx *ClusterTx) error {
		err = tx.QueryScan(q.String(), func(scan func(dest ...any) error) error {
			var loadBalancerID int64 = int64(-1)
			var backendsJSON, portsJSON string
			var loadBalancer api.NetworkLoadBalancer

			err := scan(&loadBalancerID, &loadBalancer.ListenAddress, &loadBalancer.Description, &loadBalancer.Location, &backendsJSON, &portsJSON)
			if err != nil {
				return err
			}

			loadBalancer.Backends = []api.NetworkLoadBalancerBackend{}
			if backendsJSON != "" {
				err = json.Unmarshal([]byte(backendsJSON), &loadBalancer.Backends)
				if err != nil {
					return fmt.Errorf("Failed unmarshalling backends: %w", err)
				}
			}

			loadBalancer.Ports = []api.NetworkLoadBalancerPort{}
			if portsJSON != "" {
				err = json.Unmarshal([]byte(portsJSON), &loadBalancer.Ports)
				if err != nil {
					return fmt.Errorf("Failed unmarshalling ports: %w", err)
				}
			}

			loadBalancers[loadBalancerID] = &loadBalancer

			return nil
		}, args...)
		if err != nil {
			return err
		}

		// Populate config.
		for loadBalancerID := range loadBalancers {
			err = networkLoadBalancerConfig(tx, loadBalancerID, loadBalancers[loadBalancerID])
			if err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return loadBalancers, nil
}
