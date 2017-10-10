package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/bouk/httprouter"
	"github.com/influxdata/chronograf"
	"github.com/influxdata/chronograf/influx"
)

type sourceLinks struct {
	Self        string `json:"self"`            // Self link mapping to this resource
	Kapacitors  string `json:"kapacitors"`      // URL for kapacitors endpoint
	Proxy       string `json:"proxy"`           // URL for proxy endpoint
	Queries     string `json:"queries"`         // URL for the queries analysis endpoint
	Write       string `json:"write"`           // URL for the write line-protocol endpoint
	Permissions string `json:"permissions"`     // URL for all allowed permissions for this source
	Users       string `json:"users"`           // URL for all users associated with this source
	Roles       string `json:"roles,omitempty"` // URL for all users associated with this source
	Databases   string `json:"databases"`       // URL for the databases contained within this soure
}

type sourceResponse struct {
	chronograf.Source
	Links sourceLinks `json:"links"`
}

func newSourceResponse(src chronograf.Source) sourceResponse {
	// If telegraf is not set, we'll set it to the default value.
	if src.Telegraf == "" {
		src.Telegraf = "telegraf"
	}

	// Omit the password and shared secret on response
	src.Password = ""
	src.SharedSecret = ""

	httpAPISrcs := "/chronograf/v1/sources"
	res := sourceResponse{
		Source: src,
		Links: sourceLinks{
			Self:        fmt.Sprintf("%s/%d", httpAPISrcs, src.ID),
			Kapacitors:  fmt.Sprintf("%s/%d/kapacitors", httpAPISrcs, src.ID),
			Proxy:       fmt.Sprintf("%s/%d/proxy", httpAPISrcs, src.ID),
			Queries:     fmt.Sprintf("%s/%d/queries", httpAPISrcs, src.ID),
			Write:       fmt.Sprintf("%s/%d/write", httpAPISrcs, src.ID),
			Permissions: fmt.Sprintf("%s/%d/permissions", httpAPISrcs, src.ID),
			Users:       fmt.Sprintf("%s/%d/users", httpAPISrcs, src.ID),
			Databases:   fmt.Sprintf("%s/%d/dbs", httpAPISrcs, src.ID),
		},
	}

	if src.Type == chronograf.InfluxEnterprise {
		res.Links.Roles = fmt.Sprintf("%s/%d/roles", httpAPISrcs, src.ID)
	}
	return res
}

// NewSource adds a new valid source to the store
func (h *Service) NewSource(w http.ResponseWriter, r *http.Request) {
	var src chronograf.Source
	if err := json.NewDecoder(r.Body).Decode(&src); err != nil {
		invalidJSON(w, h.Logger)
		return
	}

	if err := ValidSourceRequest(src); err != nil {
		invalidData(w, err, h.Logger)
		return
	}

	// By default the telegraf database will be telegraf
	if src.Telegraf == "" {
		src.Telegraf = "telegraf"
	}

	ctx := r.Context()
	dbType, err := h.tsdbType(ctx, &src)
	if err != nil {
		Error(w, http.StatusBadRequest, "Error contacting source", h.Logger)
		return
	}

	src.Type = dbType
	if src, err = h.SourcesStore.Add(ctx, src); err != nil {
		msg := fmt.Errorf("Error storing source %v: %v", src, err)
		unknownErrorWithMessage(w, msg, h.Logger)
		return
	}

	res := newSourceResponse(src)
	w.Header().Add("Location", res.Links.Self)
	encodeJSON(w, http.StatusCreated, res, h.Logger)
}

func (h *Service) tsdbType(ctx context.Context, src *chronograf.Source) (string, error) {
	cli := &influx.Client{
		Logger: h.Logger,
	}

	if err := cli.Connect(ctx, src); err != nil {
		return "", err
	}
	return cli.Type(ctx)
}

type getSourcesResponse struct {
	Sources []sourceResponse `json:"sources"`
}

// Sources returns all sources from the store.
func (h *Service) Sources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcs, err := h.SourcesStore.All(ctx)
	if err != nil {
		Error(w, http.StatusInternalServerError, "Error loading sources", h.Logger)
		return
	}

	res := getSourcesResponse{
		Sources: make([]sourceResponse, len(srcs)),
	}

	for i, src := range srcs {
		res.Sources[i] = newSourceResponse(src)
	}

	encodeJSON(w, http.StatusOK, res, h.Logger)
}

// SourcesID retrieves a source from the store
func (h *Service) SourcesID(w http.ResponseWriter, r *http.Request) {
	id, err := paramID("id", r)
	if err != nil {
		Error(w, http.StatusUnprocessableEntity, err.Error(), h.Logger)
		return
	}

	ctx := r.Context()
	src, err := h.SourcesStore.Get(ctx, id)
	if err != nil {
		notFound(w, id, h.Logger)
		return
	}

	res := newSourceResponse(src)
	encodeJSON(w, http.StatusOK, res, h.Logger)
}

// RemoveSource deletes the source from the store
func (h *Service) RemoveSource(w http.ResponseWriter, r *http.Request) {
	id, err := paramID("id", r)
	if err != nil {
		Error(w, http.StatusUnprocessableEntity, err.Error(), h.Logger)
		return
	}

	src := chronograf.Source{ID: id}
	ctx := r.Context()
	if err = h.SourcesStore.Delete(ctx, src); err != nil {
		if err == chronograf.ErrSourceNotFound {
			notFound(w, id, h.Logger)
		} else {
			unknownErrorWithMessage(w, err, h.Logger)
		}
		return
	}

	// Remove all the associated kapacitors for this source
	if err = h.removeSrcsKapa(ctx, id); err != nil {
		unknownErrorWithMessage(w, err, h.Logger)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// removeSrcsKapa will remove all kapacitors and kapacitor rules from the stores.
// However, it will not remove the kapacitor tickscript from kapacitor itself.
func (h *Service) removeSrcsKapa(ctx context.Context, srcID int) error {
	kapas, err := h.ServersStore.All(ctx)
	if err != nil {
		return err
	}

	// Filter the kapacitors to delete by matching the source id
	deleteKapa := []int{}
	for _, kapa := range kapas {
		if kapa.SrcID == srcID {
			deleteKapa = append(deleteKapa, kapa.ID)
		}
	}

	for _, kapaID := range deleteKapa {
		kapa := chronograf.Server{
			ID: kapaID,
		}
		h.Logger.Debug("Deleting kapacitor resource id ", kapa.ID)

		if err := h.ServersStore.Delete(ctx, kapa); err != nil {
			return err
		}
	}

	return nil
}

// UpdateSource handles incremental updates of a data source
func (h *Service) UpdateSource(w http.ResponseWriter, r *http.Request) {
	id, err := paramID("id", r)
	if err != nil {
		Error(w, http.StatusUnprocessableEntity, err.Error(), h.Logger)
		return
	}

	ctx := r.Context()
	src, err := h.SourcesStore.Get(ctx, id)
	if err != nil {
		notFound(w, id, h.Logger)
		return
	}

	var req chronograf.Source
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		invalidJSON(w, h.Logger)
		return
	}

	src.Default = req.Default
	src.InsecureSkipVerify = req.InsecureSkipVerify
	if req.Name != "" {
		src.Name = req.Name
	}
	if req.Password != "" {
		src.Password = req.Password
	}
	if req.Username != "" {
		src.Username = req.Username
	}
	if req.URL != "" {
		src.URL = req.URL
	}
	if req.MetaURL != "" {
		src.MetaURL = req.MetaURL
	}
	if req.Type != "" {
		src.Type = req.Type
	}
	if req.Telegraf != "" {
		src.Telegraf = req.Telegraf
	}

	if err := ValidSourceRequest(src); err != nil {
		invalidData(w, err, h.Logger)
		return
	}

	dbType, err := h.tsdbType(ctx, &src)
	if err != nil {
		Error(w, http.StatusBadRequest, "Error contacting source", h.Logger)
		return
	}
	src.Type = dbType

	if err := h.SourcesStore.Update(ctx, src); err != nil {
		msg := fmt.Sprintf("Error updating source ID %d", id)
		Error(w, http.StatusInternalServerError, msg, h.Logger)
		return
	}
	encodeJSON(w, http.StatusOK, newSourceResponse(src), h.Logger)
}

// ValidSourceRequest checks if name, url and type are valid
func ValidSourceRequest(s chronograf.Source) error {
	// Name and URL areq required
	if s.URL == "" {
		return fmt.Errorf("url required")
	}
	// Type must be influx or influx-enterprise
	if s.Type != "" {
		if s.Type != chronograf.InfluxDB && s.Type != chronograf.InfluxEnterprise && s.Type != chronograf.InfluxRelay {
			return fmt.Errorf("invalid source type %s", s.Type)
		}
	}

	url, err := url.ParseRequestURI(s.URL)
	if err != nil {
		return fmt.Errorf("invalid source URI: %v", err)
	}
	if len(url.Scheme) == 0 {
		return fmt.Errorf("Invalid URL; no URL scheme defined")
	}
	return nil
}

// HandleNewSources parses and persists new sources passed in via server flag
func (h *Service) HandleNewSources(ctx context.Context, input string) error {
	if input == "" {
		return nil
	}

	var srcsKaps []struct {
		Source    chronograf.Source `json:"influxdb"`
		Kapacitor chronograf.Server `json:"kapacitor"`
	}
	if err := json.Unmarshal([]byte(input), &srcsKaps); err != nil {
		h.Logger.
			WithField("component", "server").
			WithField("NewSources", "invalid").
			Error(err)
		return err
	}

	for _, sk := range srcsKaps {
		if err := ValidSourceRequest(sk.Source); err != nil {
			return err
		}
		// Add any new sources and kapacitors as specified via server flag
		if err := h.newSourceKapacitor(ctx, sk.Source, sk.Kapacitor); err != nil {
			// Continue with server run even if adding NewSource fails
			h.Logger.
				WithField("component", "server").
				WithField("NewSource", "invalid").
				Error(err)
			return err
		}
	}
	return nil
}

// newSourceKapacitor adds sources to BoltDB idempotently by name, as well as respective kapacitors
func (h *Service) newSourceKapacitor(ctx context.Context, src chronograf.Source, kapa chronograf.Server) error {
	srcs, err := h.SourcesStore.All(ctx)
	if err != nil {
		return err
	}

	for _, s := range srcs {
		// If source already exists, do nothing
		if s.Name == src.Name {
			h.Logger.
				WithField("component", "server").
				WithField("NewSource", s.Name).
				Info("Source already exists")
			return nil
		}
	}

	src, err = h.SourcesStore.Add(ctx, src)
	if err != nil {
		return err
	}

	kapa.SrcID = src.ID
	if _, err := h.ServersStore.Add(ctx, kapa); err != nil {
		return err
	}

	return nil
}

// NewSourceUser adds user to source
func (h *Service) NewSourceUser(w http.ResponseWriter, r *http.Request) {
	var req sourceUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		invalidJSON(w, h.Logger)
		return
	}

	if err := req.ValidCreate(); err != nil {
		invalidData(w, err, h.Logger)
		return
	}

	ctx := r.Context()
	srcID, ts, err := h.sourcesSeries(ctx, w, r)
	if err != nil {
		return
	}

	store := ts.Users(ctx)
	user := &chronograf.User{
		Name:        req.Username,
		Passwd:      req.Password,
		Permissions: req.Permissions,
		Roles:       req.Roles,
	}

	res, err := store.Add(ctx, user)
	if err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	if err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	su := newSourceUserResponse(srcID, res.Name).WithPermissions(res.Permissions)
	if _, hasRoles := h.hasRoles(ctx, ts); hasRoles {
		su.WithRoles(srcID, res.Roles)
	}
	w.Header().Add("Location", su.Links.Self)
	encodeJSON(w, http.StatusCreated, su, h.Logger)
}

// SourceUsers retrieves all users from source.
func (h *Service) SourceUsers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	srcID, ts, err := h.sourcesSeries(ctx, w, r)
	if err != nil {
		return
	}

	store := ts.Users(ctx)
	users, err := store.All(ctx)
	if err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	_, hasRoles := h.hasRoles(ctx, ts)
	ur := make([]sourceUserResponse, len(users))
	for i, u := range users {
		usr := newSourceUserResponse(srcID, u.Name).WithPermissions(u.Permissions)
		if hasRoles {
			usr.WithRoles(srcID, u.Roles)
		}
		ur[i] = *usr
	}

	res := sourceUsersResponse{
		Users: ur,
	}

	encodeJSON(w, http.StatusOK, res, h.Logger)
}

// SourceUserID retrieves a user with ID from store.
func (h *Service) SourceUserID(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := httprouter.GetParamFromContext(ctx, "uid")

	srcID, ts, err := h.sourcesSeries(ctx, w, r)
	if err != nil {
		return
	}
	store := ts.Users(ctx)
	u, err := store.Get(ctx, uid)
	if err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	res := newSourceUserResponse(srcID, u.Name).WithPermissions(u.Permissions)
	if _, hasRoles := h.hasRoles(ctx, ts); hasRoles {
		res.WithRoles(srcID, u.Roles)
	}
	encodeJSON(w, http.StatusOK, res, h.Logger)
}

// RemoveSourceUser removes the user from the InfluxDB source
func (h *Service) RemoveSourceUser(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	uid := httprouter.GetParamFromContext(ctx, "uid")

	_, store, err := h.sourceUsersStore(ctx, w, r)
	if err != nil {
		return
	}

	if err := store.Delete(ctx, &chronograf.User{Name: uid}); err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// UpdateSourceUser changes the password or permissions of a source user
func (h *Service) UpdateSourceUser(w http.ResponseWriter, r *http.Request) {
	var req sourceUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		invalidJSON(w, h.Logger)
		return
	}
	if err := req.ValidUpdate(); err != nil {
		invalidData(w, err, h.Logger)
		return
	}

	ctx := r.Context()
	uid := httprouter.GetParamFromContext(ctx, "uid")
	srcID, ts, err := h.sourcesSeries(ctx, w, r)
	if err != nil {
		return
	}

	user := &chronograf.User{
		Name:        uid,
		Passwd:      req.Password,
		Permissions: req.Permissions,
		Roles:       req.Roles,
	}
	store := ts.Users(ctx)

	if err := store.Update(ctx, user); err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	u, err := store.Get(ctx, uid)
	if err != nil {
		Error(w, http.StatusBadRequest, err.Error(), h.Logger)
		return
	}

	res := newSourceUserResponse(srcID, u.Name).WithPermissions(u.Permissions)
	if _, hasRoles := h.hasRoles(ctx, ts); hasRoles {
		res.WithRoles(srcID, u.Roles)
	}
	w.Header().Add("Location", res.Links.Self)
	encodeJSON(w, http.StatusOK, res, h.Logger)
}

func (h *Service) sourcesSeries(ctx context.Context, w http.ResponseWriter, r *http.Request) (int, chronograf.TimeSeries, error) {
	srcID, err := paramID("id", r)
	if err != nil {
		Error(w, http.StatusUnprocessableEntity, err.Error(), h.Logger)
		return 0, nil, err
	}

	src, err := h.SourcesStore.Get(ctx, srcID)
	if err != nil {
		notFound(w, srcID, h.Logger)
		return 0, nil, err
	}

	ts, err := h.TimeSeries(src)
	if err != nil {
		msg := fmt.Sprintf("Unable to connect to source %d: %v", srcID, err)
		Error(w, http.StatusBadRequest, msg, h.Logger)
		return 0, nil, err
	}

	if err = ts.Connect(ctx, &src); err != nil {
		msg := fmt.Sprintf("Unable to connect to source %d: %v", srcID, err)
		Error(w, http.StatusBadRequest, msg, h.Logger)
		return 0, nil, err
	}
	return srcID, ts, nil
}

func (h *Service) sourceUsersStore(ctx context.Context, w http.ResponseWriter, r *http.Request) (int, chronograf.UsersStore, error) {
	srcID, ts, err := h.sourcesSeries(ctx, w, r)
	if err != nil {
		return 0, nil, err
	}

	store := ts.Users(ctx)
	return srcID, store, nil
}

// hasRoles checks if the influx source has roles or not
func (h *Service) hasRoles(ctx context.Context, ts chronograf.TimeSeries) (chronograf.RolesStore, bool) {
	store, err := ts.Roles(ctx)
	if err != nil {
		return nil, false
	}
	return store, true
}

type sourceUserRequest struct {
	Username    string                 `json:"name,omitempty"`        // Username for new account
	Password    string                 `json:"password,omitempty"`    // Password for new account
	Permissions chronograf.Permissions `json:"permissions,omitempty"` // Optional permissions
	Roles       []chronograf.Role      `json:"roles,omitempty"`       // Optional roles
}

func (r *sourceUserRequest) ValidCreate() error {
	if r.Username == "" {
		return fmt.Errorf("Username required")
	}
	if r.Password == "" {
		return fmt.Errorf("Password required")
	}
	return validPermissions(&r.Permissions)
}

type sourceUsersResponse struct {
	Users []sourceUserResponse `json:"users"`
}

func (r *sourceUserRequest) ValidUpdate() error {
	if r.Password == "" && len(r.Permissions) == 0 && len(r.Roles) == 0 {
		return fmt.Errorf("No fields to update")
	}
	return validPermissions(&r.Permissions)
}

type sourceUserResponse struct {
	Name           string                 // Username for new account
	Permissions    chronograf.Permissions // Account's permissions
	Roles          []roleResponse         // Roles if source uses them
	Links          selfLinks              // Links are URI locations related to user
	hasPermissions bool
	hasRoles       bool
}

func (u *sourceUserResponse) MarshalJSON() ([]byte, error) {
	res := map[string]interface{}{
		"name":  u.Name,
		"links": u.Links,
	}
	if u.hasRoles {
		res["roles"] = u.Roles
	}
	if u.hasPermissions {
		res["permissions"] = u.Permissions
	}
	return json.Marshal(res)
}

// newSourceUserResponse creates an HTTP JSON response for a user w/o roles
func newSourceUserResponse(srcID int, name string) *sourceUserResponse {
	self := newSelfLinks(srcID, "users", name)
	return &sourceUserResponse{
		Name:  name,
		Links: self,
	}
}

func (u *sourceUserResponse) WithPermissions(perms chronograf.Permissions) *sourceUserResponse {
	u.hasPermissions = true
	if perms == nil {
		perms = make(chronograf.Permissions, 0)
	}
	u.Permissions = perms
	return u
}

// WithRoles adds roles to the HTTP JSON response for a user
func (u *sourceUserResponse) WithRoles(srcID int, roles []chronograf.Role) *sourceUserResponse {
	u.hasRoles = true
	rr := make([]roleResponse, len(roles))
	for i, role := range roles {
		rr[i] = newRoleResponse(srcID, &role)
	}
	u.Roles = rr
	return u
}

type selfLinks struct {
	Self string `json:"self"` // Self link mapping to this resource
}

func newSelfLinks(id int, parent, resource string) selfLinks {
	httpAPISrcs := "/chronograf/v1/sources"
	u := &url.URL{Path: resource}
	encodedResource := u.String()
	return selfLinks{
		Self: fmt.Sprintf("%s/%d/%s/%s", httpAPISrcs, id, parent, encodedResource),
	}
}
