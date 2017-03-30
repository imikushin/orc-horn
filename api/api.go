package api

import (
	"github.com/gorilla/mux"
	"github.com/rancher/go-rancher/api"
	"github.com/rancher/longhorn-orc/types"
	"net/http"
)

const Port int = 7000

func Handler(man types.VolumeManager, sl types.ServiceLocator, proxy http.Handler) http.Handler {
	r := mux.NewRouter().StrictSlash(true)
	schemas := NewSchema()
	f := api.ApiHandler
	fwd := &Fwd{sl, proxy}
	snapshots := SnapshotHandlers{man}

	versionsHandler := api.VersionsHandler(schemas, "v1")
	versionHandler := api.VersionHandler(schemas, "v1")
	r.Methods("GET").Path("/").Handler(versionsHandler)
	r.Methods("GET").Path("/v1").Handler(versionHandler)
	r.Methods("GET").Path("/v1/apiversions").Handler(versionsHandler)
	r.Methods("GET").Path("/v1/apiversions/v1").Handler(versionHandler)
	r.Methods("GET").Path("/v1/schemas").Handler(api.SchemasHandler(schemas))
	r.Methods("GET").Path("/v1/schemas/{id}").Handler(api.SchemaHandler(schemas))

	r.Methods("GET").Path("/v1/volumes/").Handler(f(schemas, VolumeListFunc(man.List)))
	r.Methods("GET").Path("/v1/volumes/{name}").Handler(f(schemas, Name2VolumeFunc(man.Get)))
	r.Methods("DELETE").Path("/v1/volumes/{name}").Handler(f(schemas, NameFunc(man.Delete)))
	r.Methods("POST").Path("/v1/volumes/").Handler(f(schemas, Volume2VolumeFunc(man.Create)))

	r.Methods("POST").Path("/v1/volumes/{name}/attach").
		Handler(f(schemas, fwd.Handler(HostIDFromAttachReq, NameFunc(man.Attach))))
	r.Methods("POST").Path("/v1/volumes/{name}/detach").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), NameFunc(man.Detach))))

	r.Methods("POST").Path("/v1/volumes/{name}/snapshots/").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), snapshots.Create)))
	r.Methods("GET").Path("/v1/volumes/{name}/snapshots/").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), snapshots.List)))
	r.Methods("GET").Path("/v1/volumes/{name}/snapshots/{snapName}").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), snapshots.Get)))
	r.Methods("DELETE").Path("/v1/volumes/{name}/snapshots/{snapName}").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), snapshots.Delete)))
	r.Methods("POST").Path("/v1/volumes/{name}/snapshots/{snapName}/revert").
		Handler(f(schemas, fwd.Handler(HostIDFromVolume(man), snapshots.Revert)))

	r.Methods("GET").Path("/v1/hosts/").Handler(f(schemas, HostListFunc(man.ListHosts)))
	r.Methods("GET").Path("/v1/hosts/{id}").Handler(f(schemas, HostGetFunc(man.GetHost)))

	return r
}
