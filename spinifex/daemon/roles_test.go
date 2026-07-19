package daemon

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFetchNATSRole_SingleNode(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"server_name": "spinifex-nats-node1",
			"jetstream": {
				"meta": {
					"leader": "",
					"cluster_size": 0
				}
			}
		}`))
	}))
	defer srv.Close()

	role := fetchNATSRole(srv.URL, srv.Client())
	assert.Equal(t, "leader", role)
}

func TestFetchNATSRole_ClusterLeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"server_name": "spinifex-nats-node1",
			"jetstream": {
				"meta": {
					"leader": "spinifex-nats-node1",
					"cluster_size": 3
				}
			}
		}`))
	}))
	defer srv.Close()

	role := fetchNATSRole(srv.URL, srv.Client())
	assert.Equal(t, "leader", role)
}

func TestFetchNATSRole_ClusterFollower(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{
			"server_name": "spinifex-nats-node2",
			"jetstream": {
				"meta": {
					"leader": "spinifex-nats-node1",
					"cluster_size": 3
				}
			}
		}`))
	}))
	defer srv.Close()

	role := fetchNATSRole(srv.URL, srv.Client())
	assert.Equal(t, "follower", role)
}

func TestFetchNATSRole_Unreachable(t *testing.T) {
	role := fetchNATSRole("http://127.0.0.1:1/varz", &http.Client{})
	assert.Empty(t, role)
}

func TestFetchNATSRole_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	role := fetchNATSRole(srv.URL, srv.Client())
	assert.Empty(t, role)
}

func TestFetchPredastoreRole_Leader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id":"1","state":"Leader","is_leader":true}`))
	}))
	defer srv.Close()

	role := fetchPredastoreRole(srv.URL, srv.Client())
	assert.Equal(t, "leader", role)
}

func TestFetchPredastoreRole_Follower(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"node_id":"2","state":"Follower","is_leader":false}`))
	}))
	defer srv.Close()

	role := fetchPredastoreRole(srv.URL, srv.Client())
	assert.Equal(t, "follower", role)
}

func TestFetchPredastoreRole_Unreachable(t *testing.T) {
	role := fetchPredastoreRole("https://127.0.0.1:1/status", &http.Client{})
	assert.Empty(t, role)
}

func TestFetchPredastoreRole_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`not json`))
	}))
	defer srv.Close()

	role := fetchPredastoreRole(srv.URL, srv.Client())
	assert.Empty(t, role)
}
