// -*- Mode: Go; indent-tabs-mode: t -*-

/*
 * Copyright (C) 2014-2018 Canonical Ltd
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License version 3 as
 * published by the Free Software Foundation.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 *
 */

package daemon

import (
	"bytes"
	"context"
	"crypto"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/sha3"
	"gopkg.in/check.v1"
	"gopkg.in/tomb.v2"

	"github.com/snapcore/snapd/arch"
	"github.com/snapcore/snapd/asserts"
	"github.com/snapcore/snapd/asserts/assertstest"
	"github.com/snapcore/snapd/asserts/sysdb"
	"github.com/snapcore/snapd/client"
	"github.com/snapcore/snapd/cmd"
	"github.com/snapcore/snapd/dirs"
	"github.com/snapcore/snapd/interfaces"
	"github.com/snapcore/snapd/interfaces/builtin"
	"github.com/snapcore/snapd/interfaces/ifacetest"
	"github.com/snapcore/snapd/osutil"
	"github.com/snapcore/snapd/overlord"
	"github.com/snapcore/snapd/overlord/assertstate"
	"github.com/snapcore/snapd/overlord/assertstate/assertstatetest"
	"github.com/snapcore/snapd/overlord/auth"
	"github.com/snapcore/snapd/overlord/configstate/config"
	"github.com/snapcore/snapd/overlord/devicestate"
	"github.com/snapcore/snapd/overlord/devicestate/devicestatetest"
	"github.com/snapcore/snapd/overlord/healthstate"
	"github.com/snapcore/snapd/overlord/hookstate"
	"github.com/snapcore/snapd/overlord/hookstate/ctlcmd"
	"github.com/snapcore/snapd/overlord/ifacestate"
	"github.com/snapcore/snapd/overlord/servicestate"
	"github.com/snapcore/snapd/overlord/snapstate"
	"github.com/snapcore/snapd/overlord/state"
	"github.com/snapcore/snapd/release"
	"github.com/snapcore/snapd/snap"
	"github.com/snapcore/snapd/snap/channel"
	"github.com/snapcore/snapd/snap/snaptest"
	"github.com/snapcore/snapd/store"
	"github.com/snapcore/snapd/store/storetest"
	"github.com/snapcore/snapd/systemd"
	"github.com/snapcore/snapd/testutil"
)

type apiBaseSuite struct {
	storetest.Store

	rsnaps            []*snap.Info
	err               error
	vars              map[string]string
	storeSearch       store.Search
	suggestedCurrency string
	d                 *Daemon
	user              *auth.UserState
	ctx               context.Context
	restoreBackends   func()
	currentSnaps      []*store.CurrentSnap
	actions           []*store.SnapAction
	buyOptions        *client.BuyOptions
	buyResult         *client.BuyResult
	storeSigning      *assertstest.StoreStack
	restoreRelease    func()
	trustedRestorer   func()
	brands            *assertstest.SigningAccounts

	systemctlRestorer func()
	sysctlArgses      [][]string
	sysctlBufs        [][]byte
	sysctlErrs        []error

	journalctlRestorer func()
	jctlSvcses         [][]string
	jctlNs             []int
	jctlFollows        []bool
	jctlRCs            []io.ReadCloser
	jctlErrs           []error

	connectivityResult     map[string]bool
	loginUserStoreMacaroon string
	loginUserDischarge     string
	userInfoResult         *store.User
	userInfoExpectedEmail  string

	restoreSanitize func()
}

func (s *apiBaseSuite) pokeStateLock() {
	// the store should be called without the state lock held. Try
	// to acquire it.
	st := s.d.overlord.State()
	st.Lock()
	st.Unlock()
}

func (s *apiBaseSuite) SnapInfo(ctx context.Context, spec store.SnapSpec, user *auth.UserState) (*snap.Info, error) {
	s.pokeStateLock()
	s.user = user
	s.ctx = ctx
	if len(s.rsnaps) > 0 {
		return s.rsnaps[0], s.err
	}
	return nil, s.err
}

func (s *apiBaseSuite) Find(ctx context.Context, search *store.Search, user *auth.UserState) ([]*snap.Info, error) {
	s.pokeStateLock()

	s.storeSearch = *search
	s.user = user
	s.ctx = ctx

	return s.rsnaps, s.err
}

func (s *apiBaseSuite) SnapAction(ctx context.Context, currentSnaps []*store.CurrentSnap, actions []*store.SnapAction, user *auth.UserState, opts *store.RefreshOptions) ([]*snap.Info, error) {
	s.pokeStateLock()

	if ctx == nil {
		panic("context required")
	}
	s.currentSnaps = currentSnaps
	s.actions = actions
	s.user = user

	return s.rsnaps, s.err
}

func (s *apiBaseSuite) SuggestedCurrency() string {
	s.pokeStateLock()

	return s.suggestedCurrency
}

func (s *apiBaseSuite) Buy(options *client.BuyOptions, user *auth.UserState) (*client.BuyResult, error) {
	s.pokeStateLock()

	s.buyOptions = options
	s.user = user
	return s.buyResult, s.err
}

func (s *apiBaseSuite) ReadyToBuy(user *auth.UserState) error {
	s.pokeStateLock()

	s.user = user
	return s.err
}

func (s *apiBaseSuite) ConnectivityCheck() (map[string]bool, error) {
	s.pokeStateLock()

	return s.connectivityResult, s.err
}

func (s *apiBaseSuite) LoginUser(username, password, otp string) (string, string, error) {
	s.pokeStateLock()

	return s.loginUserStoreMacaroon, s.loginUserDischarge, s.err
}

func (s *apiBaseSuite) UserInfo(email string) (userinfo *store.User, err error) {
	s.pokeStateLock()

	if s.userInfoExpectedEmail != email {
		panic(fmt.Sprintf("%q != %q", s.userInfoExpectedEmail, email))
	}
	return s.userInfoResult, s.err
}

func (s *apiBaseSuite) muxVars(*http.Request) map[string]string {
	return s.vars
}

func (s *apiBaseSuite) SetUpSuite(c *check.C) {
	muxVars = s.muxVars
	s.restoreRelease = release.MockForcedDevmode(false)
	s.systemctlRestorer = systemd.MockSystemctl(s.systemctl)
	s.journalctlRestorer = systemd.MockJournalctl(s.journalctl)
	s.restoreSanitize = snap.MockSanitizePlugsSlots(func(snapInfo *snap.Info) {})
}

func (s *apiBaseSuite) TearDownSuite(c *check.C) {
	muxVars = nil
	s.restoreRelease()
	s.systemctlRestorer()
	s.journalctlRestorer()
	s.restoreSanitize()
}

func (s *apiBaseSuite) systemctl(args ...string) (buf []byte, err error) {
	s.sysctlArgses = append(s.sysctlArgses, args)

	if args[0] != "show" && args[0] != "start" && args[0] != "stop" && args[0] != "restart" {
		panic(fmt.Sprintf("unexpected systemctl call: %v", args))
	}

	if len(s.sysctlErrs) > 0 {
		err, s.sysctlErrs = s.sysctlErrs[0], s.sysctlErrs[1:]
	}
	if len(s.sysctlBufs) > 0 {
		buf, s.sysctlBufs = s.sysctlBufs[0], s.sysctlBufs[1:]
	}

	return buf, err
}

func (s *apiBaseSuite) journalctl(svcs []string, n int, follow bool) (rc io.ReadCloser, err error) {
	s.jctlSvcses = append(s.jctlSvcses, svcs)
	s.jctlNs = append(s.jctlNs, n)
	s.jctlFollows = append(s.jctlFollows, follow)

	if len(s.jctlErrs) > 0 {
		err, s.jctlErrs = s.jctlErrs[0], s.jctlErrs[1:]
	}
	if len(s.jctlRCs) > 0 {
		rc, s.jctlRCs = s.jctlRCs[0], s.jctlRCs[1:]
	}

	return rc, err
}

func (s *apiBaseSuite) SetUpTest(c *check.C) {
	s.sysctlArgses = nil
	s.sysctlBufs = nil
	s.sysctlErrs = nil
	s.jctlSvcses = nil
	s.jctlNs = nil
	s.jctlFollows = nil
	s.jctlRCs = nil
	s.jctlErrs = nil

	dirs.SetRootDir(c.MkDir())
	err := os.MkdirAll(filepath.Dir(dirs.SnapStateFile), 0755)
	c.Assert(err, check.IsNil)
	c.Assert(os.MkdirAll(dirs.SnapMountDir, 0755), check.IsNil)
	c.Assert(os.MkdirAll(dirs.SnapBlobDir, 0755), check.IsNil)

	s.rsnaps = nil
	s.suggestedCurrency = ""
	s.storeSearch = store.Search{}
	s.err = nil
	s.vars = nil
	s.user = nil
	s.d = nil
	s.currentSnaps = nil
	s.actions = nil
	// Disable real security backends for all API tests
	s.restoreBackends = ifacestate.MockSecurityBackends(nil)

	s.buyOptions = nil
	s.buyResult = nil

	s.storeSigning = assertstest.NewStoreStack("can0nical", nil)
	s.trustedRestorer = sysdb.InjectTrusted(s.storeSigning.Trusted)

	s.brands = assertstest.NewSigningAccounts(s.storeSigning)
	s.brands.Register("my-brand", brandPrivKey, nil)

	assertstateRefreshSnapDeclarations = nil
	snapstateInstall = nil
	snapstateInstallMany = nil
	snapstateInstallPath = nil
	snapstateRefreshCandidates = nil
	snapstateRemoveMany = nil
	snapstateRevert = nil
	snapstateRevertToRevision = nil
	snapstateTryPath = nil
	snapstateUpdate = nil
	snapstateUpdateMany = nil
	snapstateSwitch = nil

	devicestateRemodel = nil
}

func (s *apiBaseSuite) TearDownTest(c *check.C) {
	s.trustedRestorer()
	s.d = nil
	s.ctx = nil
	s.restoreBackends()
	unsafeReadSnapInfo = unsafeReadSnapInfoImpl
	ensureStateSoon = ensureStateSoonImpl
	dirs.SetRootDir("")

	assertstateRefreshSnapDeclarations = assertstate.RefreshSnapDeclarations
	snapstateInstall = snapstate.Install
	snapstateInstallMany = snapstate.InstallMany
	snapstateInstallPath = snapstate.InstallPath
	snapstateRefreshCandidates = snapstate.RefreshCandidates
	snapstateRemoveMany = snapstate.RemoveMany
	snapstateRevert = snapstate.Revert
	snapstateRevertToRevision = snapstate.RevertToRevision
	snapstateTryPath = snapstate.TryPath
	snapstateUpdate = snapstate.Update
	snapstateUpdateMany = snapstate.UpdateMany
	snapstateSwitch = snapstate.Switch
}

var modelDefaults = map[string]interface{}{
	"architecture": "amd64",
	"gadget":       "gadget",
	"kernel":       "kernel",
}

func (s *apiBaseSuite) mockModel(c *check.C, st *state.State, model *asserts.Model) {
	// realistic model setup
	if model == nil {
		model = s.brands.Model("can0nical", "pc", modelDefaults)
	}

	snapstate.DeviceCtx = devicestate.DeviceCtx

	assertstatetest.AddMany(st, model)

	devicestatetest.SetDevice(st, &auth.DeviceState{
		Brand:  model.BrandID(),
		Model:  model.Model(),
		Serial: "serialserial",
	})
}

func (s *apiBaseSuite) daemon(c *check.C) *Daemon {
	if s.d != nil {
		panic("called daemon() twice")
	}
	d, err := New()
	c.Assert(err, check.IsNil)
	d.addRoutes()

	c.Assert(d.overlord.StartUp(), check.IsNil)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	snapstate.ReplaceStore(st, s)
	// mark as already seeded
	st.Set("seeded", true)
	// registered
	s.mockModel(c, st, nil)

	// don't actually try to talk to the store on snapstate.Ensure
	// needs doing after the call to devicestate.Manager (which
	// happens in daemon.New via overlord.New)
	snapstate.CanAutoRefresh = nil

	s.d = d
	return d
}

func (s *apiBaseSuite) daemonWithOverlordMock(c *check.C) *Daemon {
	if s.d != nil {
		panic("called daemon() twice")
	}
	d, err := New()
	c.Assert(err, check.IsNil)
	d.addRoutes()

	o := overlord.Mock()
	d.overlord = o

	st := d.overlord.State()
	// adds an assertion db
	assertstate.Manager(st, o.TaskRunner())
	st.Lock()
	defer st.Unlock()
	snapstate.ReplaceStore(st, s)

	s.d = d
	return d
}

type fakeSnapManager struct{}

func newFakeSnapManager(st *state.State, runner *state.TaskRunner) *fakeSnapManager {
	runner.AddHandler("fake-install-snap", func(t *state.Task, _ *tomb.Tomb) error {
		return nil
	}, nil)
	runner.AddHandler("fake-install-snap-error", func(t *state.Task, _ *tomb.Tomb) error {
		return fmt.Errorf("fake-install-snap-error errored")
	}, nil)

	return &fakeSnapManager{}
}

func (m *fakeSnapManager) Ensure() error {
	return nil
}

// sanity
var _ overlord.StateManager = (*fakeSnapManager)(nil)

func (s *apiBaseSuite) daemonWithFakeSnapManager(c *check.C) *Daemon {
	d := s.daemonWithOverlordMock(c)
	st := d.overlord.State()
	runner := d.overlord.TaskRunner()
	d.overlord.AddManager(newFakeSnapManager(st, runner))
	d.overlord.AddManager(runner)
	c.Assert(d.overlord.StartUp(), check.IsNil)
	return d
}

func (s *apiBaseSuite) waitTrivialChange(c *check.C, chg *state.Change) {
	err := s.d.overlord.Settle(5 * time.Second)
	c.Assert(err, check.IsNil)
	c.Assert(chg.IsReady(), check.Equals, true)
}

func (s *apiBaseSuite) mkInstalledDesktopFile(c *check.C, name, content string) string {
	df := filepath.Join(dirs.SnapDesktopFilesDir, name)
	err := os.MkdirAll(filepath.Dir(df), 0755)
	c.Assert(err, check.IsNil)
	err = ioutil.WriteFile(df, []byte(content), 0644)
	c.Assert(err, check.IsNil)
	return df
}

func (s *apiBaseSuite) mkInstalledInState(c *check.C, daemon *Daemon, instanceName, developer, version string, revision snap.Revision, active bool, extraYaml string) *snap.Info {
	snapName, instanceKey := snap.SplitInstanceName(instanceName)

	if revision.Local() && developer != "" {
		panic("not supported")
	}

	var snapID string
	if revision.Store() {
		snapID = snapName + "-id"
	}
	// Collect arguments into a snap.SideInfo structure
	sideInfo := &snap.SideInfo{
		SnapID:   snapID,
		RealName: snapName,
		Revision: revision,
		Channel:  "stable",
	}

	// Collect other arguments into a yaml string
	yamlText := fmt.Sprintf(`
name: %s
version: %s
%s`, snapName, version, extraYaml)

	// Mock the snap on disk
	snapInfo := snaptest.MockSnapInstance(c, instanceName, yamlText, sideInfo)
	if active {
		dir, rev := filepath.Split(snapInfo.MountDir())
		c.Assert(os.Symlink(rev, dir+"current"), check.IsNil)
	}
	c.Assert(snapInfo.InstanceName(), check.Equals, instanceName)

	c.Assert(os.MkdirAll(snapInfo.DataDir(), 0755), check.IsNil)
	metadir := filepath.Join(snapInfo.MountDir(), "meta")
	guidir := filepath.Join(metadir, "gui")
	c.Assert(os.MkdirAll(guidir, 0755), check.IsNil)
	c.Check(ioutil.WriteFile(filepath.Join(guidir, "icon.svg"), []byte("yadda icon"), 0644), check.IsNil)

	if daemon == nil {
		return snapInfo
	}
	st := daemon.overlord.State()
	st.Lock()
	defer st.Unlock()

	var snapst snapstate.SnapState
	snapstate.Get(st, instanceName, &snapst)
	snapst.Active = active
	snapst.Sequence = append(snapst.Sequence, &snapInfo.SideInfo)
	snapst.Current = snapInfo.SideInfo.Revision
	snapst.TrackingChannel = "stable"
	snapst.InstanceKey = instanceKey

	snapstate.Set(st, instanceName, &snapst)

	if developer == "" {
		return snapInfo
	}

	devAcct := assertstest.NewAccount(s.storeSigning, developer, map[string]interface{}{
		"account-id": developer + "-id",
	}, "")

	snapInfo.Publisher = snap.StoreAccount{
		ID:          devAcct.AccountID(),
		Username:    devAcct.Username(),
		DisplayName: devAcct.DisplayName(),
		Validation:  devAcct.Validation(),
	}

	snapDecl, err := s.storeSigning.Sign(asserts.SnapDeclarationType, map[string]interface{}{
		"series":       "16",
		"snap-id":      snapID,
		"snap-name":    snapName,
		"publisher-id": devAcct.AccountID(),
		"timestamp":    time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)

	content, err := ioutil.ReadFile(snapInfo.MountFile())
	c.Assert(err, check.IsNil)
	h := sha3.Sum384(content)
	dgst, err := asserts.EncodeDigest(crypto.SHA3_384, h[:])
	c.Assert(err, check.IsNil)
	snapRev, err := s.storeSigning.Sign(asserts.SnapRevisionType, map[string]interface{}{
		"snap-sha3-384": string(dgst),
		"snap-size":     "999",
		"snap-id":       snapID,
		"snap-revision": fmt.Sprintf("%s", revision),
		"developer-id":  devAcct.AccountID(),
		"timestamp":     time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)

	assertstatetest.AddMany(st, s.storeSigning.StoreAccountKey(""), devAcct, snapDecl, snapRev)

	return snapInfo
}

type apiSuite struct {
	apiBaseSuite
}

var _ = check.Suite(&apiSuite{})

func (s *apiSuite) TestUsersOnlyRoot(c *check.C) {
	for _, cmd := range api {
		if strings.Contains(cmd.Path, "user") {
			c.Check(cmd.RootOnly, check.Equals, true, check.Commentf(cmd.Path))
		}
	}
}

func (s *apiSuite) TestSnapInfoOneIntegration(c *check.C) {
	d := s.daemon(c)
	s.vars = map[string]string{"name": "foo"}

	// we have v0 [r5] installed
	s.mkInstalledInState(c, d, "foo", "bar", "v0", snap.R(5), false, "")
	// and v1 [r10] is current
	s.mkInstalledInState(c, d, "foo", "bar", "v1", snap.R(10), true, `title: title
description: description
summary: summary
license: GPL-3.0
base: base18
apps:
  cmd:
    command: some.cmd
  cmd2:
    command: other.cmd
  cmd3:
    command: other.cmd
    common-id: org.foo.cmd
  svc1:
    command: somed1
    daemon: simple
  svc2:
    command: somed2
    daemon: forking
  svc3:
    command: somed3
    daemon: oneshot
  svc4:
    command: somed4
    daemon: notify
  svc5:
    command: some5
    timer: mon1,12:15
    daemon: simple
  svc6:
    command: some6
    daemon: simple
    sockets:
       sock:
         listen-stream: $SNAP_COMMON/run.sock
  svc7:
    command: some7
    daemon: simple
    sockets:
       other-sock:
         listen-stream: $SNAP_COMMON/other-run.sock
`)
	df := s.mkInstalledDesktopFile(c, "foo_cmd.desktop", "[Desktop]\nExec=foo.cmd %U")
	s.sysctlBufs = [][]byte{
		[]byte(`Type=simple
Id=snap.foo.svc1.service
ActiveState=fumbling
UnitFileState=enabled
`),
		[]byte(`Type=forking
Id=snap.foo.svc2.service
ActiveState=active
UnitFileState=disabled
`),
		[]byte(`Type=oneshot
Id=snap.foo.svc3.service
ActiveState=reloading
UnitFileState=static
`),
		[]byte(`Type=notify
Id=snap.foo.svc4.service
ActiveState=inactive
UnitFileState=potatoes
`),
		[]byte(`Type=simple
Id=snap.foo.svc5.service
ActiveState=inactive
UnitFileState=static
`),
		[]byte(`Id=snap.foo.svc5.timer
ActiveState=active
UnitFileState=enabled
`),
		[]byte(`Type=simple
Id=snap.foo.svc6.service
ActiveState=inactive
UnitFileState=static
`),
		[]byte(`Id=snap.foo.svc6.sock.socket
ActiveState=active
UnitFileState=enabled
`),
		[]byte(`Type=simple
Id=snap.foo.svc7.service
ActiveState=inactive
UnitFileState=static
`),
		[]byte(`Id=snap.foo.svc7.other-sock.socket
ActiveState=inactive
UnitFileState=enabled
`),
	}

	var snapst snapstate.SnapState
	st := s.d.overlord.State()
	st.Lock()
	st.Set("health", map[string]healthstate.HealthState{
		"foo": {Status: healthstate.OkayStatus},
	})
	err := snapstate.Get(st, "foo", &snapst)
	st.Unlock()
	c.Assert(err, check.IsNil)

	// modify state
	snapst.TrackingChannel = "beta"
	snapst.IgnoreValidation = true
	snapst.CohortKey = "some-long-cohort-key"
	st.Lock()
	snapstate.Set(st, "foo", &snapst)
	st.Unlock()

	req, err := http.NewRequest("GET", "/v2/snaps/foo", nil)
	c.Assert(err, check.IsNil)
	rsp, ok := getSnapInfo(snapCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)

	c.Assert(rsp, check.NotNil)
	c.Assert(rsp.Result, check.FitsTypeOf, &client.Snap{})
	m := rsp.Result.(*client.Snap)

	// installed-size depends on vagaries of the filesystem, just check type
	c.Check(m.InstalledSize, check.FitsTypeOf, int64(0))
	m.InstalledSize = 0
	// ditto install-date
	c.Check(m.InstallDate, check.FitsTypeOf, time.Time{})
	m.InstallDate = time.Time{}

	meta := &Meta{}
	expected := &resp{
		Type:   ResponseTypeSync,
		Status: 200,
		Result: &client.Snap{
			ID:               "foo-id",
			Name:             "foo",
			Revision:         snap.R(10),
			Version:          "v1",
			Channel:          "stable",
			TrackingChannel:  "beta",
			IgnoreValidation: true,
			Title:            "title",
			Summary:          "summary",
			Description:      "description",
			Developer:        "bar",
			Publisher: &snap.StoreAccount{
				ID:          "bar-id",
				Username:    "bar",
				DisplayName: "Bar",
				Validation:  "unproven",
			},
			Status:      "active",
			Health:      &client.SnapHealth{Status: "okay"},
			Icon:        "/v2/icons/foo/icon",
			Type:        string(snap.TypeApp),
			Base:        "base18",
			Private:     false,
			DevMode:     false,
			JailMode:    false,
			Confinement: string(snap.StrictConfinement),
			TryMode:     false,
			MountedFrom: filepath.Join(dirs.SnapBlobDir, "foo_10.snap"),
			Apps: []client.AppInfo{
				{
					Snap: "foo", Name: "cmd",
					DesktopFile: df,
				}, {
					// no desktop file
					Snap: "foo", Name: "cmd2",
				}, {
					// has AppStream ID
					Snap: "foo", Name: "cmd3",
					CommonID: "org.foo.cmd",
				}, {
					// services
					Snap: "foo", Name: "svc1",
					Daemon:  "simple",
					Enabled: true,
					Active:  false,
				}, {
					Snap: "foo", Name: "svc2",
					Daemon:  "forking",
					Enabled: false,
					Active:  true,
				}, {
					Snap: "foo", Name: "svc3",
					Daemon:  "oneshot",
					Enabled: true,
					Active:  true,
				}, {
					Snap: "foo", Name: "svc4",
					Daemon:  "notify",
					Enabled: false,
					Active:  false,
				}, {
					Snap: "foo", Name: "svc5",
					Daemon:  "simple",
					Enabled: true,
					Active:  false,
					Activators: []client.AppActivator{
						{Name: "svc5", Type: "timer", Active: true, Enabled: true},
					},
				}, {
					Snap: "foo", Name: "svc6",
					Daemon:  "simple",
					Enabled: true,
					Active:  false,
					Activators: []client.AppActivator{
						{Name: "sock", Type: "socket", Active: true, Enabled: true},
					},
				}, {
					Snap: "foo", Name: "svc7",
					Daemon:  "simple",
					Enabled: true,
					Active:  false,
					Activators: []client.AppActivator{
						{Name: "other-sock", Type: "socket", Active: false, Enabled: true},
					},
				},
			},
			Broken:      "",
			Contact:     "",
			License:     "GPL-3.0",
			CommonIDs:   []string{"org.foo.cmd"},
			Screenshots: []snap.ScreenshotInfo{{Note: snap.ScreenshotsDeprecationNotice}},
			CohortKey:   "some-long-cohort-key",
		},
		Meta: meta,
	}

	c.Check(rsp.Result, check.DeepEquals, expected.Result)
}

func (s *apiSuite) TestSnapInfoWithAuth(c *check.C) {
	s.daemon(c)

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
	state.Unlock()
	c.Check(err, check.IsNil)

	req, err := http.NewRequest("GET", "/v2/find/?q=name:gfoo", nil)
	c.Assert(err, check.IsNil)

	c.Assert(s.user, check.IsNil)

	_, ok := searchStore(findCmd, req, user).(*resp)
	c.Assert(ok, check.Equals, true)
	// ensure user was set
	c.Assert(s.user, check.DeepEquals, user)
}

func (s *apiSuite) TestSnapInfoNotFound(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/snaps/gfoo", nil)
	c.Assert(err, check.IsNil)
	c.Check(getSnapInfo(snapCmd, req, nil).(*resp).Status, check.Equals, 404)
}

func (s *apiSuite) TestSnapInfoNoneFound(c *check.C) {
	s.vars = map[string]string{"name": "foo"}

	req, err := http.NewRequest("GET", "/v2/snaps/gfoo", nil)
	c.Assert(err, check.IsNil)
	c.Check(getSnapInfo(snapCmd, req, nil).(*resp).Status, check.Equals, 404)
}

func (s *apiSuite) TestSnapInfoIgnoresRemoteErrors(c *check.C) {
	s.vars = map[string]string{"name": "foo"}
	s.err = errors.New("weird")

	req, err := http.NewRequest("GET", "/v2/snaps/gfoo", nil)
	c.Assert(err, check.IsNil)
	rsp := getSnapInfo(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 404)
	c.Check(rsp.Result, check.NotNil)
}

func (s *apiSuite) TestMapLocalFields(c *check.C) {
	media := snap.MediaInfos{
		{
			Type: "screenshot",
			URL:  "https://example.com/shot1.svg",
		}, {
			Type: "icon",
			URL:  "https://example.com/icon.png",
		}, {
			Type: "screenshot",
			URL:  "https://example.com/shot2.svg",
		},
	}

	publisher := snap.StoreAccount{
		ID:          "some-dev-id",
		Username:    "some-dev",
		DisplayName: "Some Developer",
		Validation:  "poor",
	}
	info := &snap.Info{
		SideInfo: snap.SideInfo{
			SnapID:            "some-snap-id",
			RealName:          "some-snap",
			EditedTitle:       "A Title",
			EditedSummary:     "a summary",
			EditedDescription: "the\nlong\ndescription",
			Channel:           "bleeding/edge",
			Contact:           "alice@example.com",
			Revision:          snap.R(7),
			Private:           true,
		},
		InstanceKey: "instance",
		SnapType:    "app",
		Base:        "the-base",
		Version:     "v1.0",
		License:     "MIT",
		Broken:      "very",
		Confinement: "very strict",
		CommonIDs:   []string{"foo", "bar"},
		Media:       media,
		DownloadInfo: snap.DownloadInfo{
			Size:     42,
			Sha3_384: "some-sum",
		},
		Publisher: publisher,
	}

	// make InstallDate work
	c.Assert(os.MkdirAll(info.MountDir(), 0755), check.IsNil)
	c.Assert(os.Symlink("7", filepath.Join(info.MountDir(), "..", "current")), check.IsNil)

	info.Apps = map[string]*snap.AppInfo{
		"foo": {Snap: info, Name: "foo", Command: "foo"},
		"bar": {Snap: info, Name: "bar", Command: "bar"},
	}
	about := aboutSnap{
		info: info,
		snapst: &snapstate.SnapState{
			Active:          true,
			TrackingChannel: "flaky/beta",
			Current:         snap.R(7),
			Flags: snapstate.Flags{
				IgnoreValidation: true,
				DevMode:          true,
				JailMode:         true,
			},
		},
	}

	expected := &client.Snap{
		ID:               "some-snap-id",
		Name:             "some-snap_instance",
		Summary:          "a summary",
		Description:      "the\nlong\ndescription",
		Developer:        "some-dev",
		Publisher:        &publisher,
		Icon:             "https://example.com/icon.png",
		Type:             "app",
		Base:             "the-base",
		Version:          "v1.0",
		Revision:         snap.R(7),
		Channel:          "bleeding/edge",
		TrackingChannel:  "flaky/beta",
		InstallDate:      info.InstallDate(),
		InstalledSize:    42,
		Status:           "active",
		Confinement:      "very strict",
		IgnoreValidation: true,
		DevMode:          true,
		JailMode:         true,
		Private:          true,
		Broken:           "very",
		Contact:          "alice@example.com",
		Title:            "A Title",
		License:          "MIT",
		CommonIDs:        []string{"foo", "bar"},
		MountedFrom:      filepath.Join(dirs.SnapBlobDir, "some-snap_instance_7.snap"),
		Media:            media,
		Screenshots:      []snap.ScreenshotInfo{{Note: snap.ScreenshotsDeprecationNotice}},
		Apps: []client.AppInfo{
			{Snap: "some-snap_instance", Name: "bar"},
			{Snap: "some-snap_instance", Name: "foo"},
		},
	}
	c.Check(mapLocal(about), check.DeepEquals, expected)
}

func (s *apiSuite) TestMapLocalOfTryResolvesSymlink(c *check.C) {
	c.Assert(os.MkdirAll(dirs.SnapBlobDir, 0755), check.IsNil)

	info := snap.Info{SideInfo: snap.SideInfo{RealName: "hello", Revision: snap.R(1)}}
	snapst := snapstate.SnapState{}
	mountFile := info.MountFile()
	about := aboutSnap{info: &info, snapst: &snapst}

	// if not a 'try', then MountedFrom is just MountFile()
	c.Check(mapLocal(about).MountedFrom, check.Equals, mountFile)

	// if it's a try, then MountedFrom resolves the symlink
	// (note it doesn't matter, here, whether the target of the link exists)
	snapst.TryMode = true
	c.Assert(os.Symlink("/xyzzy", mountFile), check.IsNil)
	c.Check(mapLocal(about).MountedFrom, check.Equals, "/xyzzy")

	// if the readlink fails, it's unset
	c.Assert(os.Remove(mountFile), check.IsNil)
	c.Check(mapLocal(about).MountedFrom, check.Equals, "")
}

func (s *apiSuite) TestListIncludesAll(c *check.C) {
	// Very basic check to help stop us from not adding all the
	// commands to the command list.
	found := countCommandDecls(c, check.Commentf("TestListIncludesAll"))

	c.Check(found, check.Equals, len(api),
		check.Commentf(`At a glance it looks like you've not added all the Commands defined in api to the api list.`))
}

func (s *apiSuite) TestRootCmd(c *check.C) {
	// check it only does GET
	c.Check(rootCmd.PUT, check.IsNil)
	c.Check(rootCmd.POST, check.IsNil)
	c.Check(rootCmd.DELETE, check.IsNil)
	c.Assert(rootCmd.GET, check.NotNil)

	rec := httptest.NewRecorder()
	c.Check(rootCmd.Path, check.Equals, "/")

	rootCmd.GET(rootCmd, nil, nil).ServeHTTP(rec, nil)
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.HeaderMap.Get("Content-Type"), check.Equals, "application/json")

	expected := []interface{}{"TBD"}
	var rsp resp
	c.Assert(json.Unmarshal(rec.Body.Bytes(), &rsp), check.IsNil)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Result, check.DeepEquals, expected)
}

func mockSystemdVirt(newVirt string) (restore func()) {
	oldVirt := systemdVirt
	systemdVirt = newVirt
	return func() { systemdVirt = oldVirt }
}

func (s *apiSuite) TestSysInfo(c *check.C) {
	// check it only does GET
	c.Check(sysInfoCmd.PUT, check.IsNil)
	c.Check(sysInfoCmd.POST, check.IsNil)
	c.Check(sysInfoCmd.DELETE, check.IsNil)
	c.Assert(sysInfoCmd.GET, check.NotNil)

	rec := httptest.NewRecorder()
	c.Check(sysInfoCmd.Path, check.Equals, "/v2/system-info")

	d := s.daemon(c)
	d.Version = "42b1"

	// set both legacy and new refresh schedules. new one takes priority
	st := d.overlord.State()
	st.Lock()
	tr := config.NewTransaction(st)
	tr.Set("core", "refresh.schedule", "00:00-9:00/12:00-13:00")
	tr.Set("core", "refresh.timer", "8:00~9:00/2")
	tr.Commit()
	st.Unlock()

	restore := release.MockReleaseInfo(&release.OS{ID: "distro-id", VersionID: "1.2"})
	defer restore()
	restore = release.MockOnClassic(true)
	defer restore()
	restore = release.MockForcedDevmode(true)
	defer restore()
	// reload dirs for release info to have effect
	dirs.SetRootDir(dirs.GlobalRootDir)
	restore = mockSystemdVirt("magic")
	defer restore()

	buildID := "this-is-my-build-id"
	restore = MockBuildID(buildID)
	defer restore()

	sysInfoCmd.GET(sysInfoCmd, nil, nil).ServeHTTP(rec, nil)
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.HeaderMap.Get("Content-Type"), check.Equals, "application/json")

	expected := map[string]interface{}{
		"series":  "16",
		"version": "42b1",
		"os-release": map[string]interface{}{
			"id":         "distro-id",
			"version-id": "1.2",
		},
		"build-id":   buildID,
		"on-classic": true,
		"managed":    false,
		"locations": map[string]interface{}{
			"snap-mount-dir": dirs.SnapMountDir,
			"snap-bin-dir":   dirs.SnapBinariesDir,
		},
		"refresh": map[string]interface{}{
			// only the "timer" field
			"timer": "8:00~9:00/2",
		},
		"confinement":      "partial",
		"sandbox-features": map[string]interface{}{"confinement-options": []interface{}{"classic", "devmode"}},
		"architecture":     arch.DpkgArchitecture(),
		"virtualization":   "magic",
	}
	var rsp resp
	c.Assert(json.Unmarshal(rec.Body.Bytes(), &rsp), check.IsNil)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	// Ensure that we had a kernel-verrsion but don't check the actual value.
	const kernelVersionKey = "kernel-version"
	c.Check(rsp.Result.(map[string]interface{})[kernelVersionKey], check.Not(check.Equals), "")
	delete(rsp.Result.(map[string]interface{}), kernelVersionKey)
	c.Check(rsp.Result, check.DeepEquals, expected)
}

func (s *apiSuite) TestSysInfoLegacyRefresh(c *check.C) {
	rec := httptest.NewRecorder()

	d := s.daemon(c)
	d.Version = "42b1"

	restore := release.MockReleaseInfo(&release.OS{ID: "distro-id", VersionID: "1.2"})
	defer restore()
	restore = release.MockOnClassic(true)
	defer restore()
	restore = release.MockForcedDevmode(true)
	defer restore()
	restore = mockSystemdVirt("kvm")
	defer restore()
	// reload dirs for release info to have effect
	dirs.SetRootDir(dirs.GlobalRootDir)

	// set the legacy refresh schedule
	st := d.overlord.State()
	st.Lock()
	tr := config.NewTransaction(st)
	tr.Set("core", "refresh.schedule", "00:00-9:00/12:00-13:00")
	tr.Set("core", "refresh.timer", "")
	tr.Commit()
	st.Unlock()

	// add a test security backend
	err := d.overlord.InterfaceManager().Repository().AddBackend(&ifacetest.TestSecurityBackend{
		BackendName:             "apparmor",
		SandboxFeaturesCallback: func() []string { return []string{"feature-1", "feature-2"} },
	})
	c.Assert(err, check.IsNil)

	buildID := "this-is-my-build-id"
	restore = MockBuildID(buildID)
	defer restore()

	sysInfoCmd.GET(sysInfoCmd, nil, nil).ServeHTTP(rec, nil)
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.HeaderMap.Get("Content-Type"), check.Equals, "application/json")

	expected := map[string]interface{}{
		"series":  "16",
		"version": "42b1",
		"os-release": map[string]interface{}{
			"id":         "distro-id",
			"version-id": "1.2",
		},
		"build-id":   buildID,
		"on-classic": true,
		"managed":    false,
		"locations": map[string]interface{}{
			"snap-mount-dir": dirs.SnapMountDir,
			"snap-bin-dir":   dirs.SnapBinariesDir,
		},
		"refresh": map[string]interface{}{
			// only the "schedule" field
			"schedule": "00:00-9:00/12:00-13:00",
		},
		"confinement": "partial",
		"sandbox-features": map[string]interface{}{
			"apparmor":            []interface{}{"feature-1", "feature-2"},
			"confinement-options": []interface{}{"classic", "devmode"}, // we know it's this because of the release.Mock... calls above
		},
		"architecture":   arch.DpkgArchitecture(),
		"virtualization": "kvm",
	}
	var rsp resp
	c.Assert(json.Unmarshal(rec.Body.Bytes(), &rsp), check.IsNil)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	const kernelVersionKey = "kernel-version"
	delete(rsp.Result.(map[string]interface{}), kernelVersionKey)
	c.Check(rsp.Result, check.DeepEquals, expected)
}

func (s *apiSuite) TestLoginUser(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()

	s.loginUserStoreMacaroon = "user-macaroon"
	s.loginUserDischarge = "the-discharge-macaroon-serialized-data"
	buf := bytes.NewBufferString(`{"username": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(loginCmd, req, nil).(*resp)

	state.Lock()
	user, err := auth.User(state, 1)
	state.Unlock()
	c.Check(err, check.IsNil)

	expected := userResponseData{
		ID:    1,
		Email: "email@.com",

		Macaroon:   user.Macaroon,
		Discharges: user.Discharges,
	}

	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, expected)
	c.Check(rsp.Result, check.DeepEquals, expected)

	c.Check(user.ID, check.Equals, 1)
	c.Check(user.Username, check.Equals, "")
	c.Check(user.Email, check.Equals, "email@.com")
	c.Check(user.Discharges, check.IsNil)
	c.Check(user.StoreMacaroon, check.Equals, s.loginUserStoreMacaroon)
	c.Check(user.StoreDischarges, check.DeepEquals, []string{"the-discharge-macaroon-serialized-data"})
	// snapd macaroon was setup too
	snapdMacaroon, err := auth.MacaroonDeserialize(user.Macaroon)
	c.Check(err, check.IsNil)
	c.Check(snapdMacaroon.Id(), check.Equals, "1")
	c.Check(snapdMacaroon.Location(), check.Equals, "snapd")
}

func (s *apiSuite) TestLoginUserWithUsername(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()

	s.loginUserStoreMacaroon = "user-macaroon"
	s.loginUserDischarge = "the-discharge-macaroon-serialized-data"
	buf := bytes.NewBufferString(`{"username": "username", "email": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(loginCmd, req, nil).(*resp)

	state.Lock()
	user, err := auth.User(state, 1)
	state.Unlock()
	c.Check(err, check.IsNil)

	expected := userResponseData{
		ID:         1,
		Username:   "username",
		Email:      "email@.com",
		Macaroon:   user.Macaroon,
		Discharges: user.Discharges,
	}
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, expected)
	c.Check(rsp.Result, check.DeepEquals, expected)

	c.Check(user.ID, check.Equals, 1)
	c.Check(user.Username, check.Equals, "username")
	c.Check(user.Email, check.Equals, "email@.com")
	c.Check(user.Discharges, check.IsNil)
	c.Check(user.StoreMacaroon, check.Equals, s.loginUserStoreMacaroon)
	c.Check(user.StoreDischarges, check.DeepEquals, []string{"the-discharge-macaroon-serialized-data"})
	// snapd macaroon was setup too
	snapdMacaroon, err := auth.MacaroonDeserialize(user.Macaroon)
	c.Check(err, check.IsNil)
	c.Check(snapdMacaroon.Id(), check.Equals, "1")
	c.Check(snapdMacaroon.Location(), check.Equals, "snapd")
}

func (s *apiSuite) TestLoginUserNoEmailWithExistentLocalUser(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()

	// setup local-only user
	state.Lock()
	localUser, err := auth.NewUser(state, "username", "email@test.com", "", nil)
	state.Unlock()
	c.Assert(err, check.IsNil)

	s.loginUserStoreMacaroon = "user-macaroon"
	s.loginUserDischarge = "the-discharge-macaroon-serialized-data"
	buf := bytes.NewBufferString(`{"username": "username", "email": "", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", fmt.Sprintf(`Macaroon root="%s"`, localUser.Macaroon))

	rsp := loginUser(loginCmd, req, localUser).(*resp)

	expected := userResponseData{
		ID:       1,
		Username: "username",
		Email:    "email@test.com",

		Macaroon:   localUser.Macaroon,
		Discharges: localUser.Discharges,
	}
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, expected)
	c.Check(rsp.Result, check.DeepEquals, expected)

	state.Lock()
	user, err := auth.User(state, localUser.ID)
	state.Unlock()
	c.Check(err, check.IsNil)
	c.Check(user.Username, check.Equals, "username")
	c.Check(user.Email, check.Equals, localUser.Email)
	c.Check(user.Macaroon, check.Equals, localUser.Macaroon)
	c.Check(user.Discharges, check.IsNil)
	c.Check(user.StoreMacaroon, check.Equals, s.loginUserStoreMacaroon)
	c.Check(user.StoreDischarges, check.DeepEquals, []string{"the-discharge-macaroon-serialized-data"})
}

func (s *apiSuite) TestLoginUserWithExistentLocalUser(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()

	// setup local-only user
	state.Lock()
	localUser, err := auth.NewUser(state, "username", "email@test.com", "", nil)
	state.Unlock()
	c.Assert(err, check.IsNil)

	s.loginUserStoreMacaroon = "user-macaroon"
	s.loginUserDischarge = "the-discharge-macaroon-serialized-data"
	buf := bytes.NewBufferString(`{"username": "username", "email": "email@test.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", fmt.Sprintf(`Macaroon root="%s"`, localUser.Macaroon))

	rsp := loginUser(loginCmd, req, localUser).(*resp)

	expected := userResponseData{
		ID:       1,
		Username: "username",
		Email:    "email@test.com",

		Macaroon:   localUser.Macaroon,
		Discharges: localUser.Discharges,
	}
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, expected)
	c.Check(rsp.Result, check.DeepEquals, expected)

	state.Lock()
	user, err := auth.User(state, localUser.ID)
	state.Unlock()
	c.Check(err, check.IsNil)
	c.Check(user.Username, check.Equals, "username")
	c.Check(user.Email, check.Equals, localUser.Email)
	c.Check(user.Macaroon, check.Equals, localUser.Macaroon)
	c.Check(user.Discharges, check.IsNil)
	c.Check(user.StoreMacaroon, check.Equals, s.loginUserStoreMacaroon)
	c.Check(user.StoreDischarges, check.DeepEquals, []string{"the-discharge-macaroon-serialized-data"})
}

func (s *apiSuite) TestLoginUserNewEmailWithExistentLocalUser(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()

	// setup local-only user
	state.Lock()
	localUser, err := auth.NewUser(state, "username", "email@test.com", "", nil)
	state.Unlock()
	c.Assert(err, check.IsNil)

	s.loginUserStoreMacaroon = "user-macaroon"
	s.loginUserDischarge = "the-discharge-macaroon-serialized-data"
	// same local user, but using a new SSO account
	buf := bytes.NewBufferString(`{"username": "username", "email": "new.email@test.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", fmt.Sprintf(`Macaroon root="%s"`, localUser.Macaroon))

	rsp := loginUser(loginCmd, req, localUser).(*resp)

	expected := userResponseData{
		ID:       1,
		Username: "username",
		Email:    "new.email@test.com",

		Macaroon:   localUser.Macaroon,
		Discharges: localUser.Discharges,
	}
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, expected)
	c.Check(rsp.Result, check.DeepEquals, expected)

	state.Lock()
	user, err := auth.User(state, localUser.ID)
	state.Unlock()
	c.Check(err, check.IsNil)
	c.Check(user.Username, check.Equals, "username")
	c.Check(user.Email, check.Equals, expected.Email)
	c.Check(user.Macaroon, check.Equals, localUser.Macaroon)
	c.Check(user.Discharges, check.IsNil)
	c.Check(user.StoreMacaroon, check.Equals, s.loginUserStoreMacaroon)
	c.Check(user.StoreDischarges, check.DeepEquals, []string{"the-discharge-macaroon-serialized-data"})
}

func (s *apiSuite) TestLogoutUser(c *check.C) {
	d := s.daemon(c)
	state := d.overlord.State()
	state.Lock()
	user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
	state.Unlock()
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/logout", nil)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", `Macaroon root="macaroon", discharge="discharge"`)

	rsp := logoutUser(logoutCmd, req, user).(*resp)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)

	state.Lock()
	_, err = auth.User(state, user.ID)
	state.Unlock()
	c.Check(err, check.Equals, auth.ErrInvalidUser)
}

func (s *apiSuite) TestLoginUserBadRequest(c *check.C) {
	buf := bytes.NewBufferString(`hello`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result, check.NotNil)
}

func (s *apiSuite) TestLoginUserDeveloperAPIError(c *check.C) {
	s.daemon(c)

	s.err = fmt.Errorf("error-from-login-user")
	buf := bytes.NewBufferString(`{"username": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 401)
	c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, "error-from-login-user")
}

func (s *apiSuite) TestLoginUserTwoFactorRequiredError(c *check.C) {
	s.daemon(c)

	s.err = store.ErrAuthenticationNeeds2fa
	buf := bytes.NewBufferString(`{"username": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 401)
	c.Check(rsp.Result.(*errorResult).Kind, check.Equals, errorKindTwoFactorRequired)
}

func (s *apiSuite) TestLoginUserTwoFactorFailedError(c *check.C) {
	s.daemon(c)

	s.err = store.Err2faFailed
	buf := bytes.NewBufferString(`{"username": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 401)
	c.Check(rsp.Result.(*errorResult).Kind, check.Equals, errorKindTwoFactorFailed)
}

func (s *apiSuite) TestLoginUserInvalidCredentialsError(c *check.C) {
	s.daemon(c)

	s.err = store.ErrInvalidCredentials
	buf := bytes.NewBufferString(`{"username": "email@.com", "password": "password"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)

	rsp := loginUser(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 401)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, "invalid credentials")
}

func (s *apiSuite) TestUserFromRequestNoHeader(c *check.C) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := UserFromRequest(state, req)
	state.Unlock()

	c.Check(err, check.Equals, auth.ErrInvalidAuth)
	c.Check(user, check.IsNil)
}

func (s *apiSuite) TestUserFromRequestHeaderNoMacaroons(c *check.C) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Authorization", "Invalid")

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := UserFromRequest(state, req)
	state.Unlock()

	c.Check(err, check.ErrorMatches, "authorization header misses Macaroon prefix")
	c.Check(user, check.IsNil)
}

func (s *apiSuite) TestUserFromRequestHeaderIncomplete(c *check.C) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Authorization", `Macaroon root=""`)

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := UserFromRequest(state, req)
	state.Unlock()

	c.Check(err, check.ErrorMatches, "invalid authorization header")
	c.Check(user, check.IsNil)
}

func (s *apiSuite) TestUserFromRequestHeaderCorrectMissingUser(c *check.C) {
	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Authorization", `Macaroon root="macaroon", discharge="discharge"`)

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := UserFromRequest(state, req)
	state.Unlock()

	c.Check(err, check.Equals, auth.ErrInvalidAuth)
	c.Check(user, check.IsNil)
}

func (s *apiSuite) TestUserFromRequestHeaderValidUser(c *check.C) {
	state := snapCmd.d.overlord.State()
	state.Lock()
	expectedUser, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
	state.Unlock()
	c.Check(err, check.IsNil)

	req, _ := http.NewRequest("GET", "http://example.com", nil)
	req.Header.Set("Authorization", fmt.Sprintf(`Macaroon root="%s"`, expectedUser.Macaroon))

	state.Lock()
	user, err := UserFromRequest(state, req)
	state.Unlock()

	c.Check(err, check.IsNil)
	c.Check(user, check.DeepEquals, expectedUser)
}

func (s *apiSuite) TestSnapsInfoOnePerIntegration(c *check.C) {
	s.checkSnapInfoOnePerIntegration(c, false, nil)
}

func (s *apiSuite) TestSnapsInfoOnePerIntegrationSome(c *check.C) {
	s.checkSnapInfoOnePerIntegration(c, false, []string{"foo", "baz"})
}

func (s *apiSuite) TestSnapsInfoOnePerIntegrationAll(c *check.C) {
	s.checkSnapInfoOnePerIntegration(c, true, nil)
}

func (s *apiSuite) TestSnapsInfoOnePerIntegrationAllSome(c *check.C) {
	s.checkSnapInfoOnePerIntegration(c, true, []string{"foo", "baz"})
}

func (s *apiSuite) checkSnapInfoOnePerIntegration(c *check.C, all bool, names []string) {
	d := s.daemon(c)

	type tsnap struct {
		name   string
		dev    string
		ver    string
		rev    int
		active bool

		wanted bool
	}

	tsnaps := []tsnap{
		{name: "foo", dev: "bar", ver: "v0.9", rev: 1},
		{name: "foo", dev: "bar", ver: "v1", rev: 5, active: true},
		{name: "bar", dev: "baz", ver: "v2", rev: 10, active: true},
		{name: "baz", dev: "qux", ver: "v3", rev: 15, active: true},
		{name: "qux", dev: "mip", ver: "v4", rev: 20, active: true},
	}
	numExpected := 0

	for _, snp := range tsnaps {
		if all || snp.active {
			if len(names) == 0 {
				numExpected++
				snp.wanted = true
			}
			for _, n := range names {
				if snp.name == n {
					numExpected++
					snp.wanted = true
					break
				}
			}
		}
		s.mkInstalledInState(c, d, snp.name, snp.dev, snp.ver, snap.R(snp.rev), snp.active, "")
	}

	q := url.Values{}
	if all {
		q.Set("select", "all")
	}
	if len(names) > 0 {
		q.Set("snaps", strings.Join(names, ","))
	}
	req, err := http.NewRequest("GET", "/v2/snaps?"+q.Encode(), nil)
	c.Assert(err, check.IsNil)

	rsp, ok := getSnapsInfo(snapsCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)

	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Result, check.NotNil)

	snaps := snapList(rsp.Result)
	c.Check(snaps, check.HasLen, numExpected)

	for _, s := range tsnaps {
		if !((all || s.active) && s.wanted) {
			continue
		}
		var got map[string]interface{}
		for _, got = range snaps {
			if got["name"].(string) == s.name && got["revision"].(string) == snap.R(s.rev).String() {
				break
			}
		}
		c.Check(got["name"], check.Equals, s.name)
		c.Check(got["version"], check.Equals, s.ver)
		c.Check(got["revision"], check.Equals, snap.R(s.rev).String())
		c.Check(got["developer"], check.Equals, s.dev)
		c.Check(got["confinement"], check.Equals, "strict")
	}
}

func (s *apiSuite) TestSnapsInfoOnlyLocal(c *check.C) {
	d := s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	s.mkInstalledInState(c, d, "local", "foo", "v1", snap.R(10), true, "")
	st := s.d.overlord.State()
	st.Lock()
	st.Set("health", map[string]healthstate.HealthState{
		"local": {Status: healthstate.OkayStatus},
	})
	st.Unlock()

	req, err := http.NewRequest("GET", "/v2/snaps?sources=local", nil)
	c.Assert(err, check.IsNil)

	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)

	c.Assert(rsp.Sources, check.DeepEquals, []string{"local"})

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Assert(snaps[0]["name"], check.Equals, "local")
	c.Check(snaps[0]["health"], check.DeepEquals, map[string]interface{}{
		"status":    "okay",
		"revision":  "unset",
		"timestamp": "0001-01-01T00:00:00Z",
	})
}

func (s *apiSuite) TestSnapsInfoAllMixedPublishers(c *check.C) {
	d := s.daemon(c)

	// the first 'local' is from a 'local' snap
	s.mkInstalledInState(c, d, "local", "", "v1", snap.R(-1), false, "")
	s.mkInstalledInState(c, d, "local", "foo", "v2", snap.R(1), false, "")
	s.mkInstalledInState(c, d, "local", "foo", "v3", snap.R(2), true, "")

	req, err := http.NewRequest("GET", "/v2/snaps?select=all", nil)
	c.Assert(err, check.IsNil)
	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeSync)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 3)

	publisher := map[string]interface{}{
		"id":           "foo-id",
		"username":     "foo",
		"display-name": "Foo",
		"validation":   "unproven",
	}

	c.Check(snaps[0]["publisher"], check.IsNil)
	c.Check(snaps[1]["publisher"], check.DeepEquals, publisher)
	c.Check(snaps[2]["publisher"], check.DeepEquals, publisher)
}

func (s *apiSuite) TestSnapsInfoAll(c *check.C) {
	d := s.daemon(c)

	s.mkInstalledInState(c, d, "local", "foo", "v1", snap.R(1), false, "")
	s.mkInstalledInState(c, d, "local", "foo", "v2", snap.R(2), false, "")
	s.mkInstalledInState(c, d, "local", "foo", "v3", snap.R(3), true, "")
	s.mkInstalledInState(c, d, "local_foo", "foo", "v4", snap.R(4), true, "")
	brokenInfo := s.mkInstalledInState(c, d, "local_bar", "foo", "v5", snap.R(5), true, "")
	// make sure local_bar is 'broken'
	err := os.Remove(filepath.Join(brokenInfo.MountDir(), "meta", "snap.yaml"))
	c.Assert(err, check.IsNil)

	expectedHappy := map[string]bool{
		"local":     true,
		"local_foo": true,
		"local_bar": true,
	}
	for _, t := range []struct {
		q        string
		numSnaps int
		typ      ResponseType
	}{
		{"?select=enabled", 3, "sync"},
		{`?select=`, 3, "sync"},
		{"", 3, "sync"},
		{"?select=all", 5, "sync"},
		{"?select=invalid-field", 0, "error"},
	} {
		c.Logf("trying: %v", t)
		req, err := http.NewRequest("GET", fmt.Sprintf("/v2/snaps%s", t.q), nil)
		c.Assert(err, check.IsNil)
		rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)
		c.Assert(rsp.Type, check.Equals, t.typ)

		if rsp.Type != "error" {
			snaps := snapList(rsp.Result)
			c.Assert(snaps, check.HasLen, t.numSnaps)
			seen := map[string]bool{}
			for _, s := range snaps {
				seen[s["name"].(string)] = true
			}
			c.Assert(seen, check.DeepEquals, expectedHappy)
		}
	}
}

func (s *apiSuite) TestFind(c *check.C) {
	s.daemon(c)

	s.suggestedCurrency = "EUR"

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}

	req, err := http.NewRequest("GET", "/v2/find?q=hi", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Assert(snaps[0]["name"], check.Equals, "store")
	c.Check(snaps[0]["prices"], check.IsNil)
	c.Check(snaps[0]["screenshots"], check.DeepEquals, []interface{}{map[string]interface{}{"note": snap.ScreenshotsDeprecationNotice}})
	c.Check(snaps[0]["channels"], check.IsNil)

	c.Check(rsp.SuggestedCurrency, check.Equals, "EUR")

	c.Check(s.storeSearch, check.DeepEquals, store.Search{Query: "hi"})
	c.Check(s.currentSnaps, check.HasLen, 0)
	c.Check(s.actions, check.HasLen, 0)
}

func (s *apiSuite) TestFindRefreshes(c *check.C) {
	snapstateRefreshCandidates = snapstate.RefreshCandidates
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	s.mockSnap(c, "name: store\nversion: 1.0")

	req, err := http.NewRequest("GET", "/v2/find?select=refresh", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Assert(snaps[0]["name"], check.Equals, "store")
	c.Check(s.currentSnaps, check.HasLen, 1)
	c.Check(s.actions, check.HasLen, 1)
}

func (s *apiSuite) TestFindRefreshSideloaded(c *check.C) {
	snapstateRefreshCandidates = snapstate.RefreshCandidates
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}

	s.mockSnap(c, "name: store\nversion: 1.0")

	var snapst snapstate.SnapState
	st := s.d.overlord.State()
	st.Lock()
	err := snapstate.Get(st, "store", &snapst)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Assert(snapst.Sequence, check.HasLen, 1)

	// clear the snapid
	snapst.Sequence[0].SnapID = ""
	st.Lock()
	snapstate.Set(st, "store", &snapst)
	st.Unlock()

	req, err := http.NewRequest("GET", "/v2/find?select=refresh", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 0)
	c.Check(s.currentSnaps, check.HasLen, 0)
	c.Check(s.actions, check.HasLen, 0)
}

func (s *apiSuite) TestFindPrivate(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{}

	req, err := http.NewRequest("GET", "/v2/find?q=foo&select=private", nil)
	c.Assert(err, check.IsNil)

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{
		Query:   "foo",
		Private: true,
	})
}

func (s *apiSuite) TestFindUserAgentContextCreated(c *check.C) {
	s.daemon(c)

	req, err := http.NewRequest("GET", "/v2/find", nil)
	c.Assert(err, check.IsNil)
	req.Header.Add("User-Agent", "some-agent/1.0")

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(store.ClientUserAgent(s.ctx), check.Equals, "some-agent/1.0")
}

func (s *apiSuite) TestFindOneUserAgentContextCreated(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SnapType: snap.TypeApp,
		Version:  "v2",
		SideInfo: snap.SideInfo{
			RealName: "banana",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	req, err := http.NewRequest("GET", "/v2/find?name=foo", nil)
	c.Assert(err, check.IsNil)
	req.Header.Add("User-Agent", "some-agent/1.0")

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(store.ClientUserAgent(s.ctx), check.Equals, "some-agent/1.0")
}

func (s *apiSuite) TestFindPrefix(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{}

	req, err := http.NewRequest("GET", "/v2/find?name=foo*", nil)
	c.Assert(err, check.IsNil)

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{Query: "foo", Prefix: true})
}

func (s *apiSuite) TestFindSection(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{}

	req, err := http.NewRequest("GET", "/v2/find?q=foo&section=bar", nil)
	c.Assert(err, check.IsNil)

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{
		Query:   "foo",
		Section: "bar",
	})
}

func (s *apiSuite) TestFindScope(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{}

	req, err := http.NewRequest("GET", "/v2/find?q=foo&scope=creep", nil)
	c.Assert(err, check.IsNil)

	_ = searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{
		Query: "foo",
		Scope: "creep",
	})
}

func (s *apiSuite) TestFindCommonID(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
		CommonIDs: []string{"org.foo"},
	}}
	s.mockSnap(c, "name: store\nversion: 1.0")

	req, err := http.NewRequest("GET", "/v2/find?name=foo", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(snaps[0]["common-ids"], check.DeepEquals, []interface{}{"org.foo"})
}

func (s *apiSuite) TestFindByCommonID(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
		CommonIDs: []string{"org.foo"},
	}}
	s.mockSnap(c, "name: store\nversion: 1.0")

	req, err := http.NewRequest("GET", "/v2/find?common-id=org.foo", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(s.storeSearch, check.DeepEquals, store.Search{CommonID: "org.foo"})
}

func (s *apiSuite) TestFindOne(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Base: "base0",
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "verified",
		},
		Channels: map[string]*snap.ChannelSnapInfo{
			"stable": {
				Revision: snap.R(42),
			},
		},
	}}
	s.mockSnap(c, "name: store\nversion: 1.0")

	req, err := http.NewRequest("GET", "/v2/find?name=foo", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{})

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(snaps[0]["name"], check.Equals, "store")
	c.Check(snaps[0]["base"], check.Equals, "base0")
	c.Check(snaps[0]["publisher"], check.DeepEquals, map[string]interface{}{
		"id":           "foo-id",
		"username":     "foo",
		"display-name": "Foo",
		"validation":   "verified",
	})
	m := snaps[0]["channels"].(map[string]interface{})["stable"].(map[string]interface{})

	c.Check(m["revision"], check.Equals, "42")
}

func (s *apiSuite) TestFindOneNotFound(c *check.C) {
	s.daemon(c)

	s.err = store.ErrSnapNotFound
	s.mockSnap(c, "name: store\nversion: 1.0")

	req, err := http.NewRequest("GET", "/v2/find?name=foo", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{})
	c.Check(rsp.Status, check.Equals, 404)
}

func (s *apiSuite) TestFindRefreshNotOther(c *check.C) {
	for _, other := range []string{"name", "q", "common-id"} {
		req, err := http.NewRequest("GET", "/v2/find?select=refresh&"+other+"=foo*", nil)
		c.Assert(err, check.IsNil)

		rsp := searchStore(findCmd, req, nil).(*resp)
		c.Check(rsp.Type, check.Equals, ResponseTypeError)
		c.Check(rsp.Status, check.Equals, 400)
		c.Check(rsp.Result.(*errorResult).Message, check.Equals, "cannot use '"+other+"' with 'select=refresh'")
	}
}

func (s *apiSuite) TestFindNotTogether(c *check.C) {
	queries := map[string]string{"q": "foo", "name": "foo*", "common-id": "foo"}
	for ki, vi := range queries {
		for kj, vj := range queries {
			if ki == kj {
				continue
			}

			req, err := http.NewRequest("GET", fmt.Sprintf("/v2/find?%s=%s&%s=%s", ki, vi, kj, vj), nil)
			c.Assert(err, check.IsNil)

			rsp := searchStore(findCmd, req, nil).(*resp)
			c.Check(rsp.Type, check.Equals, ResponseTypeError)
			c.Check(rsp.Status, check.Equals, 400)
			exp1 := "cannot use '" + ki + "' and '" + kj + "' together"
			exp2 := "cannot use '" + kj + "' and '" + ki + "' together"
			c.Check(rsp.Result.(*errorResult).Message, check.Matches, exp1+"|"+exp2)
		}
	}
}

func (s *apiSuite) TestFindBadQueryReturnsCorrectErrorKind(c *check.C) {
	s.daemon(c)

	s.err = store.ErrBadQuery
	req, err := http.NewRequest("GET", "/v2/find?q=return-bad-query-please", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result.(*errorResult).Message, check.Matches, "bad query")
	c.Check(rsp.Result.(*errorResult).Kind, check.Equals, errorKindBadQuery)
}

func (s *apiSuite) TestFindPriced(c *check.C) {
	s.daemon(c)

	s.suggestedCurrency = "GBP"

	s.rsnaps = []*snap.Info{{
		SnapType: snap.TypeApp,
		Version:  "v2",
		Prices: map[string]float64{
			"GBP": 1.23,
			"EUR": 2.34,
		},
		MustBuy: true,
		SideInfo: snap.SideInfo{
			RealName: "banana",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}

	req, err := http.NewRequest("GET", "/v2/find?q=banana&channel=stable", nil)
	c.Assert(err, check.IsNil)
	rsp, ok := searchStore(findCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)

	snap := snaps[0]
	c.Check(snap["name"], check.Equals, "banana")
	c.Check(snap["prices"], check.DeepEquals, map[string]interface{}{
		"EUR": 2.34,
		"GBP": 1.23,
	})
	c.Check(snap["status"], check.Equals, "priced")

	c.Check(rsp.SuggestedCurrency, check.Equals, "GBP")
}

func (s *apiSuite) TestFindScreenshotted(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SnapType: snap.TypeApp,
		Version:  "v2",
		Media: []snap.MediaInfo{
			{
				Type:   "screenshot",
				URL:    "http://example.com/screenshot.png",
				Width:  800,
				Height: 1280,
			},
			{
				Type: "screenshot",
				URL:  "http://example.com/screenshot2.png",
			},
		},
		MustBuy: true,
		SideInfo: snap.SideInfo{
			RealName: "test-screenshot",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}

	req, err := http.NewRequest("GET", "/v2/find?q=test-screenshot", nil)
	c.Assert(err, check.IsNil)
	rsp, ok := searchStore(findCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)

	c.Check(snaps[0]["name"], check.Equals, "test-screenshot")
	c.Check(snaps[0]["screenshots"], check.DeepEquals, []interface{}{
		map[string]interface{}{
			"note": snap.ScreenshotsDeprecationNotice,
		},
	})
	c.Check(snaps[0]["media"], check.DeepEquals, []interface{}{
		map[string]interface{}{
			"type":   "screenshot",
			"url":    "http://example.com/screenshot.png",
			"width":  float64(800),
			"height": float64(1280),
		},
		map[string]interface{}{
			"type": "screenshot",
			"url":  "http://example.com/screenshot2.png",
		},
	})
}

func (s *apiSuite) TestSnapsInfoOnlyStore(c *check.C) {
	d := s.daemon(c)

	s.suggestedCurrency = "EUR"

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "store",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	s.mkInstalledInState(c, d, "local", "foo", "v1", snap.R(10), true, "")

	req, err := http.NewRequest("GET", "/v2/snaps?sources=store", nil)
	c.Assert(err, check.IsNil)

	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)

	c.Assert(rsp.Sources, check.DeepEquals, []string{"store"})

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Assert(snaps[0]["name"], check.Equals, "store")
	c.Check(snaps[0]["prices"], check.IsNil)

	c.Check(rsp.SuggestedCurrency, check.Equals, "EUR")
}

func (s *apiSuite) TestSnapsStoreConfinement(c *check.C) {
	s.daemon(c)

	s.rsnaps = []*snap.Info{
		{
			// no explicit confinement in this one
			SideInfo: snap.SideInfo{
				RealName: "foo",
			},
		},
		{
			Confinement: snap.StrictConfinement,
			SideInfo: snap.SideInfo{
				RealName: "bar",
			},
		},
		{
			Confinement: snap.DevModeConfinement,
			SideInfo: snap.SideInfo{
				RealName: "baz",
			},
		},
	}

	req, err := http.NewRequest("GET", "/v2/find", nil)
	c.Assert(err, check.IsNil)

	rsp := searchStore(findCmd, req, nil).(*resp)

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 3)

	for i, ss := range [][2]string{
		{"foo", string(snap.StrictConfinement)},
		{"bar", string(snap.StrictConfinement)},
		{"baz", string(snap.DevModeConfinement)},
	} {
		name, mode := ss[0], ss[1]
		c.Check(snaps[i]["name"], check.Equals, name, check.Commentf(name))
		c.Check(snaps[i]["confinement"], check.Equals, mode, check.Commentf(name))
	}
}

func (s *apiSuite) TestSnapsInfoStoreWithAuth(c *check.C) {
	s.daemon(c)

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
	state.Unlock()
	c.Check(err, check.IsNil)

	req, err := http.NewRequest("GET", "/v2/snaps?sources=store", nil)
	c.Assert(err, check.IsNil)

	c.Assert(s.user, check.IsNil)

	_ = getSnapsInfo(snapsCmd, req, user).(*resp)

	// ensure user was set
	c.Assert(s.user, check.DeepEquals, user)
}

func (s *apiSuite) TestSnapsInfoLocalAndStore(c *check.C) {
	d := s.daemon(c)

	s.rsnaps = []*snap.Info{{
		Version: "v42",
		SideInfo: snap.SideInfo{
			RealName: "remote",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	s.mkInstalledInState(c, d, "local", "foo", "v1", snap.R(10), true, "")

	req, err := http.NewRequest("GET", "/v2/snaps?sources=local,store", nil)
	c.Assert(err, check.IsNil)

	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)

	// presence of 'store' in sources bounces request over to /find
	c.Assert(rsp.Sources, check.DeepEquals, []string{"store"})

	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(snaps[0]["version"], check.Equals, "v42")

	// as does a 'q'
	req, err = http.NewRequest("GET", "/v2/snaps?q=what", nil)
	c.Assert(err, check.IsNil)
	rsp = getSnapsInfo(snapsCmd, req, nil).(*resp)
	snaps = snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(snaps[0]["version"], check.Equals, "v42")

	// otherwise, local only
	req, err = http.NewRequest("GET", "/v2/snaps", nil)
	c.Assert(err, check.IsNil)
	rsp = getSnapsInfo(snapsCmd, req, nil).(*resp)
	snaps = snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
	c.Check(snaps[0]["version"], check.Equals, "v1")
}

func (s *apiSuite) TestSnapsInfoDefaultSources(c *check.C) {
	d := s.daemon(c)

	s.rsnaps = []*snap.Info{{
		SideInfo: snap.SideInfo{
			RealName: "remote",
		},
		Publisher: snap.StoreAccount{
			ID:          "foo-id",
			Username:    "foo",
			DisplayName: "Foo",
			Validation:  "unproven",
		},
	}}
	s.mkInstalledInState(c, d, "local", "foo", "v1", snap.R(10), true, "")

	req, err := http.NewRequest("GET", "/v2/snaps", nil)
	c.Assert(err, check.IsNil)

	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)

	c.Assert(rsp.Sources, check.DeepEquals, []string{"local"})
	snaps := snapList(rsp.Result)
	c.Assert(snaps, check.HasLen, 1)
}

func (s *apiSuite) TestSnapsInfoFilterRemote(c *check.C) {
	s.daemon(c)

	s.rsnaps = nil

	req, err := http.NewRequest("GET", "/v2/snaps?q=foo&sources=store", nil)
	c.Assert(err, check.IsNil)

	rsp := getSnapsInfo(snapsCmd, req, nil).(*resp)

	c.Check(s.storeSearch, check.DeepEquals, store.Search{Query: "foo"})

	c.Assert(rsp.Result, check.NotNil)
}

func (s *apiSuite) TestPostSnapBadRequest(c *check.C) {
	buf := bytes.NewBufferString(`hello`)
	req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
	c.Assert(err, check.IsNil)

	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result, check.NotNil)
}

func (s *apiSuite) TestPostSnapBadAction(c *check.C) {
	buf := bytes.NewBufferString(`{"action": "potato"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
	c.Assert(err, check.IsNil)

	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result, check.NotNil)
}

func (s *apiSuite) TestPostSnap(c *check.C) {
	d := s.daemonWithOverlordMock(c)

	soon := 0
	ensureStateSoon = func(st *state.State) {
		soon++
		ensureStateSoonImpl(st)
	}

	s.vars = map[string]string{"name": "foo"}

	snapInstructionDispTable["install"] = func(*snapInstruction, *state.State) (string, []*state.TaskSet, error) {
		return "foooo", nil, nil
	}
	defer func() {
		snapInstructionDispTable["install"] = snapInstall
	}()

	buf := bytes.NewBufferString(`{"action": "install"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
	c.Assert(err, check.IsNil)

	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeAsync)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)
	c.Check(chg.Summary(), check.Equals, "foooo")
	var names []string
	err = chg.Get("snap-names", &names)
	c.Assert(err, check.IsNil)
	c.Check(names, check.DeepEquals, []string{"foo"})

	c.Check(soon, check.Equals, 1)
}

func (s *apiSuite) TestPostSnapVerifySnapInstruction(c *check.C) {
	s.daemonWithOverlordMock(c)

	buf := bytes.NewBufferString(`{"action": "install"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/ubuntu-core", buf)
	c.Assert(err, check.IsNil)
	s.vars = map[string]string{"name": "ubuntu-core"}

	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, `cannot install "ubuntu-core", please use "core" instead`)
}

func (s *apiSuite) TestPostSnapCohortRandoAction(c *check.C) {
	s.daemonWithOverlordMock(c)
	s.vars = map[string]string{"name": "some-snap"}
	const expectedErr = "cohort-key can only be specified for install, refresh, or switch"

	for _, action := range []string{"remove", "revert", "enable", "disable", "xyzzy"} {
		buf := strings.NewReader(fmt.Sprintf(`{"action": "%s", "cohort-key": "32"}`, action))
		req, err := http.NewRequest("POST", "/v2/snaps/some-snap", buf)
		c.Assert(err, check.IsNil)

		rsp := postSnap(snapCmd, req, nil).(*resp)

		c.Check(rsp.Type, check.Equals, ResponseTypeError)
		c.Check(rsp.Status, check.Equals, 400, check.Commentf("%q", action))
		c.Check(rsp.Result.(*errorResult).Message, check.Equals, expectedErr, check.Commentf("%q", action))
	}
}

func (s *apiSuite) TestPostSnapLeaveCohortRandoAction(c *check.C) {
	s.daemonWithOverlordMock(c)
	s.vars = map[string]string{"name": "some-snap"}
	const expectedErr = "leave-cohort can only be specified for refresh or switch"

	for _, action := range []string{"install", "remove", "revert", "enable", "disable", "xyzzy"} {
		buf := strings.NewReader(fmt.Sprintf(`{"action": "%s", "leave-cohort": true}`, action))
		req, err := http.NewRequest("POST", "/v2/snaps/some-snap", buf)
		c.Assert(err, check.IsNil)

		rsp := postSnap(snapCmd, req, nil).(*resp)

		c.Check(rsp.Type, check.Equals, ResponseTypeError)
		c.Check(rsp.Status, check.Equals, 400, check.Commentf("%q", action))
		c.Check(rsp.Result.(*errorResult).Message, check.Equals, expectedErr, check.Commentf("%q", action))
	}
}

func (s *apiSuite) TestPostSnapCohortIncompat(c *check.C) {
	s.daemonWithOverlordMock(c)
	s.vars = map[string]string{"name": "some-snap"}

	type T struct {
		opts   string
		errmsg string
	}

	for i, t := range []T{
		// TODO: more?
		{`"cohort-key": "what", "revision": "42"`, `cannot specify both cohort-key and revision`},
		{`"cohort-key": "what", "leave-cohort": true`, `cannot specify both cohort-key and leave-cohort`},
	} {
		buf := strings.NewReader(fmt.Sprintf(`{"action": "refresh", %s}`, t.opts))
		req, err := http.NewRequest("POST", "/v2/snaps/some-snap", buf)
		c.Assert(err, check.IsNil, check.Commentf("%d (%s)", i, t.opts))

		rsp := postSnap(snapCmd, req, nil).(*resp)

		c.Check(rsp.Type, check.Equals, ResponseTypeError, check.Commentf("%d (%s)", i, t.opts))
		c.Check(rsp.Status, check.Equals, 400, check.Commentf("%d (%s)", i, t.opts))
		c.Check(rsp.Result.(*errorResult).Message, check.Equals, t.errmsg, check.Commentf("%d (%s)", i, t.opts))
	}
}

func (s *apiSuite) TestPostSnapVerifyMultiSnapInstruction(c *check.C) {
	s.daemonWithOverlordMock(c)

	buf := strings.NewReader(`{"action": "install","snaps":["ubuntu-core"]}`)
	req, err := http.NewRequest("POST", "/v2/snaps", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "application/json")

	rsp := postSnaps(snapsCmd, req, nil).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, `cannot install "ubuntu-core", please use "core" instead`)
}

func (s *apiSuite) TestPostSnapsNoWeirdses(c *check.C) {
	s.daemonWithOverlordMock(c)

	// one could add more actions here ... 🤷
	for _, action := range []string{"install", "refresh", "remove"} {
		for weird, v := range map[string]string{
			"channel":      `"beta"`,
			"revision":     `"1"`,
			"devmode":      "true",
			"jailmode":     "true",
			"cohort-key":   `"what"`,
			"leave-cohort": "true",
		} {
			buf := strings.NewReader(fmt.Sprintf(`{"action": "%s","snaps":["foo","bar"], "%s": %s}`, action, weird, v))
			req, err := http.NewRequest("POST", "/v2/snaps", buf)
			c.Assert(err, check.IsNil)
			req.Header.Set("Content-Type", "application/json")

			rsp := postSnaps(snapsCmd, req, nil).(*resp)

			c.Check(rsp.Type, check.Equals, ResponseTypeError)
			c.Check(rsp.Status, check.Equals, 400)
			c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, `unsupported option provided for multi-snap operation`)
		}
	}
}

func (s *apiSuite) TestPostSnapSetsUser(c *check.C) {
	d := s.daemon(c)
	ensureStateSoon = func(st *state.State) {}

	snapInstructionDispTable["install"] = func(inst *snapInstruction, st *state.State) (string, []*state.TaskSet, error) {
		return fmt.Sprintf("<install by user %d>", inst.userID), nil, nil
	}
	defer func() {
		snapInstructionDispTable["install"] = snapInstall
	}()

	state := snapCmd.d.overlord.State()
	state.Lock()
	user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
	state.Unlock()
	c.Check(err, check.IsNil)

	buf := bytes.NewBufferString(`{"action": "install"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Authorization", `Macaroon root="macaroon", discharge="discharge"`)

	rsp := postSnap(snapCmd, req, user).(*resp)

	c.Check(rsp.Type, check.Equals, ResponseTypeAsync)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)
	c.Check(chg.Summary(), check.Equals, "<install by user 1>")
}

func (s *apiSuite) TestPostSnapDispatch(c *check.C) {
	inst := &snapInstruction{Snaps: []string{"foo"}}

	type T struct {
		s    string
		impl snapActionFunc
	}

	actions := []T{
		{"install", snapInstall},
		{"refresh", snapUpdate},
		{"remove", snapRemove},
		{"revert", snapRevert},
		{"enable", snapEnable},
		{"disable", snapDisable},
		{"switch", snapSwitch},
		{"xyzzy", nil},
	}

	for _, action := range actions {
		inst.Action = action.s
		// do you feel dirty yet?
		c.Check(fmt.Sprintf("%p", action.impl), check.Equals, fmt.Sprintf("%p", inst.dispatch()))
	}
}

func (s *apiSuite) TestPostSnapEnableDisableSwitchRevision(c *check.C) {
	for _, action := range []string{"enable", "disable", "switch"} {
		buf := bytes.NewBufferString(`{"action": "` + action + `", "revision": "42"}`)
		req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
		c.Assert(err, check.IsNil)

		rsp := postSnap(snapCmd, req, nil).(*resp)

		c.Check(rsp.Type, check.Equals, ResponseTypeError)
		c.Check(rsp.Status, check.Equals, 400)
		c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, "takes no revision")
	}
}

var sideLoadBodyWithoutDevMode = "" +
	"----hello--\r\n" +
	"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
	"\r\n" +
	"xyzzy\r\n" +
	"----hello--\r\n" +
	"Content-Disposition: form-data; name=\"dangerous\"\r\n" +
	"\r\n" +
	"true\r\n" +
	"----hello--\r\n" +
	"Content-Disposition: form-data; name=\"snap-path\"\r\n" +
	"\r\n" +
	"a/b/local.snap\r\n" +
	"----hello--\r\n"

func (s *apiSuite) TestSideloadSnapOnNonDevModeDistro(c *check.C) {
	// try a multipart/form-data upload
	body := sideLoadBodyWithoutDevMode
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	chgSummary := s.sideloadCheck(c, body, head, "local", snapstate.Flags{RemoveSnapPath: true})
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "a/b/local.snap"`)
}

func (s *apiSuite) TestSideloadSnapOnDevModeDistro(c *check.C) {
	// try a multipart/form-data upload
	body := sideLoadBodyWithoutDevMode
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	restore := release.MockForcedDevmode(true)
	defer restore()
	flags := snapstate.Flags{RemoveSnapPath: true}
	chgSummary := s.sideloadCheck(c, body, head, "local", flags)
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "a/b/local.snap"`)
}

func (s *apiSuite) TestSideloadSnapDevMode(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"devmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	// try a multipart/form-data upload
	flags := snapstate.Flags{RemoveSnapPath: true}
	flags.DevMode = true
	chgSummary := s.sideloadCheck(c, body, head, "local", flags)
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "x"`)
}

func (s *apiSuite) TestSideloadSnapJailMode(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"jailmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"dangerous\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	// try a multipart/form-data upload
	flags := snapstate.Flags{JailMode: true, RemoveSnapPath: true}
	chgSummary := s.sideloadCheck(c, body, head, "local", flags)
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "x"`)
}

func (s *apiSuite) sideloadCheck(c *check.C, content string, head map[string]string, expectedInstanceName string, expectedFlags snapstate.Flags) string {
	d := s.daemonWithFakeSnapManager(c)

	soon := 0
	ensureStateSoon = func(st *state.State) {
		soon++
		ensureStateSoonImpl(st)
	}

	c.Assert(expectedInstanceName != "", check.Equals, true, check.Commentf("expected instance name must be set"))
	mockedName, _ := snap.SplitInstanceName(expectedInstanceName)

	// setup done
	installQueue := []string{}
	unsafeReadSnapInfo = func(path string) (*snap.Info, error) {
		return &snap.Info{SuggestedName: mockedName}, nil
	}

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		// NOTE: ubuntu-core is not installed in developer mode
		c.Check(flags, check.Equals, snapstate.Flags{})
		installQueue = append(installQueue, name)

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	snapstateInstallPath = func(s *state.State, si *snap.SideInfo, path, name, channel string, flags snapstate.Flags) (*state.TaskSet, *snap.Info, error) {
		c.Check(flags, check.DeepEquals, expectedFlags)

		c.Check(path, testutil.FileEquals, "xyzzy")

		c.Check(name, check.Equals, expectedInstanceName)

		installQueue = append(installQueue, si.RealName+"::"+path)
		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), &snap.Info{SuggestedName: name}, nil
	}

	buf := bytes.NewBufferString(content)
	req, err := http.NewRequest("POST", "/v2/snaps", buf)
	c.Assert(err, check.IsNil)
	for k, v := range head {
		req.Header.Set(k, v)
	}

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeAsync)
	n := 1
	c.Assert(installQueue, check.HasLen, n)
	c.Check(installQueue[n-1], check.Matches, "local::.*/"+regexp.QuoteMeta(dirs.LocalInstallBlobTempPrefix)+".*")

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)

	c.Check(soon, check.Equals, 1)

	c.Assert(chg.Tasks(), check.HasLen, n)

	st.Unlock()
	s.waitTrivialChange(c, chg)
	st.Lock()

	c.Check(chg.Kind(), check.Equals, "install-snap")
	var names []string
	err = chg.Get("snap-names", &names)
	c.Assert(err, check.IsNil)
	c.Check(names, check.DeepEquals, []string{expectedInstanceName})
	var apiData map[string]interface{}
	err = chg.Get("api-data", &apiData)
	c.Assert(err, check.IsNil)
	c.Check(apiData, check.DeepEquals, map[string]interface{}{
		"snap-name": expectedInstanceName,
	})

	return chg.Summary()
}

func (s *apiSuite) TestSideloadSnapJailModeAndDevmode(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"jailmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"devmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	s.daemonWithOverlordMock(c)

	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, "cannot use devmode and jailmode flags together")
}

func (s *apiSuite) TestSideloadSnapJailModeInDevModeOS(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"jailmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	s.daemonWithOverlordMock(c)

	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	restore := release.MockForcedDevmode(true)
	defer restore()

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, "this system cannot honour the jailmode flag")
}

func (s *apiSuite) TestLocalInstallSnapDeriveSideInfo(c *check.C) {
	d := s.daemonWithOverlordMock(c)
	// add the assertions first
	st := d.overlord.State()

	dev1Acct := assertstest.NewAccount(s.storeSigning, "devel1", nil, "")

	snapDecl, err := s.storeSigning.Sign(asserts.SnapDeclarationType, map[string]interface{}{
		"series":       "16",
		"snap-id":      "x-id",
		"snap-name":    "x",
		"publisher-id": dev1Acct.AccountID(),
		"timestamp":    time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)

	snapRev, err := s.storeSigning.Sign(asserts.SnapRevisionType, map[string]interface{}{
		"snap-sha3-384": "YK0GWATaZf09g_fvspYPqm_qtaiqf-KjaNj5uMEQCjQpuXWPjqQbeBINL5H_A0Lo",
		"snap-size":     "5",
		"snap-id":       "x-id",
		"snap-revision": "41",
		"developer-id":  dev1Acct.AccountID(),
		"timestamp":     time.Now().Format(time.RFC3339),
	}, nil, "")
	c.Assert(err, check.IsNil)

	func() {
		st.Lock()
		defer st.Unlock()
		assertstatetest.AddMany(st, s.storeSigning.StoreAccountKey(""), dev1Acct, snapDecl, snapRev)
	}()

	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x.snap\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n"
	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	snapstateInstallPath = func(s *state.State, si *snap.SideInfo, path, name, channel string, flags snapstate.Flags) (*state.TaskSet, *snap.Info, error) {
		c.Check(flags, check.Equals, snapstate.Flags{RemoveSnapPath: true})
		c.Check(si, check.DeepEquals, &snap.SideInfo{
			RealName: "x",
			SnapID:   "x-id",
			Revision: snap.R(41),
		})

		return state.NewTaskSet(), &snap.Info{SuggestedName: "x"}, nil
	}

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeAsync)

	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)
	c.Check(chg.Summary(), check.Equals, `Install "x" snap from file "x.snap"`)
	var names []string
	err = chg.Get("snap-names", &names)
	c.Assert(err, check.IsNil)
	c.Check(names, check.DeepEquals, []string{"x"})
	var apiData map[string]interface{}
	err = chg.Get("api-data", &apiData)
	c.Assert(err, check.IsNil)
	c.Check(apiData, check.DeepEquals, map[string]interface{}{
		"snap-name": "x",
	})
}

func (s *apiSuite) TestSideloadSnapNoSignaturesDangerOff(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n"
	s.daemonWithOverlordMock(c)

	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	// this is the prefix used for tempfiles for sideloading
	glob := filepath.Join(os.TempDir(), "snapd-sideload-pkg-*")
	glbBefore, _ := filepath.Glob(glob)
	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `cannot find signatures with metadata for snap "x"`)
	glbAfter, _ := filepath.Glob(glob)
	c.Check(len(glbBefore), check.Equals, len(glbAfter))
}

func (s *apiSuite) TestSideloadSnapNotValidFormFile(c *check.C) {
	newTestDaemon(c)

	// try a multipart/form-data upload with missing "name"
	content := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}

	buf := bytes.NewBufferString(content)
	req, err := http.NewRequest("POST", "/v2/snaps", buf)
	c.Assert(err, check.IsNil)
	for k, v := range head {
		req.Header.Set(k, v)
	}

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Assert(rsp.Result.(*errorResult).Message, check.Matches, `cannot find "snap" file field in provided multipart/form-data payload`)
}

func (s *apiSuite) TestSideloadSnapChangeConflict(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"dangerous\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	s.daemonWithOverlordMock(c)

	unsafeReadSnapInfo = func(path string) (*snap.Info, error) {
		return &snap.Info{SuggestedName: "foo"}, nil
	}

	snapstateInstallPath = func(s *state.State, si *snap.SideInfo, path, name, channel string, flags snapstate.Flags) (*state.TaskSet, *snap.Info, error) {
		return nil, nil, &snapstate.ChangeConflictError{Snap: "foo"}
	}

	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Kind, check.Equals, errorKindSnapChangeConflict)
}

func (s *apiSuite) TestSideloadSnapInstanceName(c *check.C) {
	// try a multipart/form-data upload
	body := sideLoadBodyWithoutDevMode +
		"Content-Disposition: form-data; name=\"name\"\r\n" +
		"\r\n" +
		"local_instance\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	chgSummary := s.sideloadCheck(c, body, head, "local_instance", snapstate.Flags{RemoveSnapPath: true})
	c.Check(chgSummary, check.Equals, `Install "local_instance" snap from file "a/b/local.snap"`)
}

func (s *apiSuite) TestSideloadSnapInstanceNameNoKey(c *check.C) {
	// try a multipart/form-data upload
	body := sideLoadBodyWithoutDevMode +
		"Content-Disposition: form-data; name=\"name\"\r\n" +
		"\r\n" +
		"local\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	chgSummary := s.sideloadCheck(c, body, head, "local", snapstate.Flags{RemoveSnapPath: true})
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "a/b/local.snap"`)
}

func (s *apiSuite) TestSideloadSnapInstanceNameMismatch(c *check.C) {
	s.daemonWithFakeSnapManager(c)

	unsafeReadSnapInfo = func(path string) (*snap.Info, error) {
		return &snap.Info{SuggestedName: "bar"}, nil
	}

	body := sideLoadBodyWithoutDevMode +
		"Content-Disposition: form-data; name=\"name\"\r\n" +
		"\r\n" +
		"foo_instance\r\n" +
		"----hello--\r\n"

	req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(body))
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "multipart/thing; boundary=--hello--")

	rsp := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `instance name "foo_instance" does not match snap name "bar"`)
}

func (s *apiSuite) TestTrySnap(c *check.C) {
	d := s.daemonWithFakeSnapManager(c)

	var err error

	// mock a try dir
	tryDir := c.MkDir()
	snapYaml := filepath.Join(tryDir, "meta", "snap.yaml")
	err = os.MkdirAll(filepath.Dir(snapYaml), 0755)
	c.Assert(err, check.IsNil)
	err = ioutil.WriteFile(snapYaml, []byte("name: foo\nversion: 1.0\n"), 0644)
	c.Assert(err, check.IsNil)

	reqForFlags := func(f snapstate.Flags) *http.Request {
		b := "" +
			"--hello\r\n" +
			"Content-Disposition: form-data; name=\"action\"\r\n" +
			"\r\n" +
			"try\r\n" +
			"--hello\r\n" +
			"Content-Disposition: form-data; name=\"snap-path\"\r\n" +
			"\r\n" +
			tryDir + "\r\n" +
			"--hello"

		snip := "\r\n" +
			"Content-Disposition: form-data; name=%q\r\n" +
			"\r\n" +
			"true\r\n" +
			"--hello"

		if f.DevMode {
			b += fmt.Sprintf(snip, "devmode")
		}
		if f.JailMode {
			b += fmt.Sprintf(snip, "jailmode")
		}
		if f.Classic {
			b += fmt.Sprintf(snip, "classic")
		}
		b += "--\r\n"

		req, err := http.NewRequest("POST", "/v2/snaps", bytes.NewBufferString(b))
		c.Assert(err, check.IsNil)
		req.Header.Set("Content-Type", "multipart/thing; boundary=hello")

		return req
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()

	for _, t := range []struct {
		flags snapstate.Flags
		desc  string
	}{
		{snapstate.Flags{}, "core; -"},
		{snapstate.Flags{DevMode: true}, "core; devmode"},
		{snapstate.Flags{JailMode: true}, "core; jailmode"},
		{snapstate.Flags{Classic: true}, "core; classic"},
	} {
		soon := 0
		ensureStateSoon = func(st *state.State) {
			soon++
			ensureStateSoonImpl(st)
		}

		tryWasCalled := true
		snapstateTryPath = func(s *state.State, name, path string, flags snapstate.Flags) (*state.TaskSet, error) {
			c.Check(flags, check.DeepEquals, t.flags, check.Commentf(t.desc))
			tryWasCalled = true
			t := s.NewTask("fake-install-snap", "Doing a fake try")
			return state.NewTaskSet(t), nil
		}

		snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
			if name != "core" {
				c.Check(flags, check.DeepEquals, t.flags, check.Commentf(t.desc))
			}
			t := s.NewTask("fake-install-snap", "Doing a fake install")
			return state.NewTaskSet(t), nil
		}

		// try the snap (without an installed core)
		st.Unlock()
		rsp := postSnaps(snapsCmd, reqForFlags(t.flags), nil).(*resp)
		st.Lock()
		c.Assert(rsp.Type, check.Equals, ResponseTypeAsync, check.Commentf(t.desc))
		c.Assert(tryWasCalled, check.Equals, true, check.Commentf(t.desc))

		chg := st.Change(rsp.Change)
		c.Assert(chg, check.NotNil, check.Commentf(t.desc))

		c.Assert(chg.Tasks(), check.HasLen, 1, check.Commentf(t.desc))

		st.Unlock()
		s.waitTrivialChange(c, chg)
		st.Lock()

		c.Check(chg.Kind(), check.Equals, "try-snap", check.Commentf(t.desc))
		c.Check(chg.Summary(), check.Equals, fmt.Sprintf(`Try "%s" snap from %s`, "foo", tryDir), check.Commentf(t.desc))
		var names []string
		err = chg.Get("snap-names", &names)
		c.Assert(err, check.IsNil, check.Commentf(t.desc))
		c.Check(names, check.DeepEquals, []string{"foo"}, check.Commentf(t.desc))
		var apiData map[string]interface{}
		err = chg.Get("api-data", &apiData)
		c.Assert(err, check.IsNil, check.Commentf(t.desc))
		c.Check(apiData, check.DeepEquals, map[string]interface{}{
			"snap-name": "foo",
		}, check.Commentf(t.desc))

		c.Check(soon, check.Equals, 1, check.Commentf(t.desc))
	}
}

func (s *apiSuite) TestTrySnapRelative(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/snaps", nil)
	c.Assert(err, check.IsNil)

	rsp := trySnap(snapsCmd, req, nil, "relative-path", snapstate.Flags{}).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, "need an absolute path")
}

func (s *apiSuite) TestTrySnapNotDir(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/snaps", nil)
	c.Assert(err, check.IsNil)

	rsp := trySnap(snapsCmd, req, nil, "/does/not/exist", snapstate.Flags{}).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, testutil.Contains, "not a snap directory")
}

func (s *apiSuite) TestTryChangeConflict(c *check.C) {
	s.daemonWithOverlordMock(c)

	// mock a try dir
	tryDir := c.MkDir()

	unsafeReadSnapInfo = func(path string) (*snap.Info, error) {
		return &snap.Info{SuggestedName: "foo"}, nil
	}

	snapstateTryPath = func(s *state.State, name, path string, flags snapstate.Flags) (*state.TaskSet, error) {
		return nil, &snapstate.ChangeConflictError{Snap: "foo"}
	}

	req, err := http.NewRequest("POST", "/v2/snaps", nil)
	c.Assert(err, check.IsNil)

	rsp := trySnap(snapsCmd, req, nil, tryDir, snapstate.Flags{}).(*resp)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Kind, check.Equals, errorKindSnapChangeConflict)
}

func (s *apiSuite) runGetConf(c *check.C, snapName string, keys []string, statusCode int) map[string]interface{} {
	s.vars = map[string]string{"name": snapName}
	req, err := http.NewRequest("GET", "/v2/snaps/"+snapName+"/conf?keys="+strings.Join(keys, ","), nil)
	c.Check(err, check.IsNil)
	rec := httptest.NewRecorder()
	snapConfCmd.GET(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, statusCode)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	return body["result"].(map[string]interface{})
}

func (s *apiSuite) TestGetConfSingleKey(c *check.C) {
	d := s.daemon(c)

	// Set a config that we'll get in a moment
	d.overlord.State().Lock()
	tr := config.NewTransaction(d.overlord.State())
	tr.Set("test-snap", "test-key1", "test-value1")
	tr.Set("test-snap", "test-key2", "test-value2")
	tr.Commit()
	d.overlord.State().Unlock()

	result := s.runGetConf(c, "test-snap", []string{"test-key1"}, 200)
	c.Check(result, check.DeepEquals, map[string]interface{}{"test-key1": "test-value1"})

	result = s.runGetConf(c, "test-snap", []string{"test-key1", "test-key2"}, 200)
	c.Check(result, check.DeepEquals, map[string]interface{}{"test-key1": "test-value1", "test-key2": "test-value2"})
}

func (s *apiSuite) TestGetConfCoreSystemAlias(c *check.C) {
	d := s.daemon(c)

	// Set a config that we'll get in a moment
	d.overlord.State().Lock()
	tr := config.NewTransaction(d.overlord.State())
	tr.Set("core", "test-key1", "test-value1")
	tr.Commit()
	d.overlord.State().Unlock()

	result := s.runGetConf(c, "core", []string{"test-key1"}, 200)
	c.Check(result, check.DeepEquals, map[string]interface{}{"test-key1": "test-value1"})

	result = s.runGetConf(c, "system", []string{"test-key1"}, 200)
	c.Check(result, check.DeepEquals, map[string]interface{}{"test-key1": "test-value1"})
}

func (s *apiSuite) TestGetConfMissingKey(c *check.C) {
	result := s.runGetConf(c, "test-snap", []string{"test-key2"}, 400)
	c.Check(result, check.DeepEquals, map[string]interface{}{
		"value": map[string]interface{}{
			"SnapName": "test-snap",
			"Key":      "test-key2",
		},
		"message": `snap "test-snap" has no "test-key2" configuration option`,
		"kind":    "option-not-found",
	})
}

func (s *apiSuite) TestGetRootDocument(c *check.C) {
	d := s.daemon(c)
	d.overlord.State().Lock()
	tr := config.NewTransaction(d.overlord.State())
	tr.Set("test-snap", "test-key1", "test-value1")
	tr.Set("test-snap", "test-key2", "test-value2")
	tr.Commit()
	d.overlord.State().Unlock()

	result := s.runGetConf(c, "test-snap", nil, 200)
	c.Check(result, check.DeepEquals, map[string]interface{}{"test-key1": "test-value1", "test-key2": "test-value2"})
}

func (s *apiSuite) TestGetConfBadKey(c *check.C) {
	s.daemon(c)
	// TODO: this one in particular should really be a 400 also
	result := s.runGetConf(c, "test-snap", []string{"."}, 500)
	c.Check(result, check.DeepEquals, map[string]interface{}{"message": `invalid option name: ""`})
}

func (s *apiSuite) TestSetConf(c *check.C) {
	d := s.daemon(c)
	s.mockSnap(c, configYaml)

	// Mock the hook runner
	hookRunner := testutil.MockCommand(c, "snap", "")
	defer hookRunner.Restore()

	d.overlord.Loop()
	defer d.overlord.Stop()

	text, err := json.Marshal(map[string]interface{}{"key": "value"})
	c.Assert(err, check.IsNil)

	buffer := bytes.NewBuffer(text)
	req, err := http.NewRequest("PUT", "/v2/snaps/config-snap/conf", buffer)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "config-snap"}

	rec := httptest.NewRecorder()
	snapConfCmd.PUT(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	// Check that the configure hook was run correctly
	c.Check(hookRunner.Calls(), check.DeepEquals, [][]string{{
		"snap", "run", "--hook", "configure", "-r", "unset", "config-snap",
	}})
}

func (s *apiSuite) TestSetConfCoreSystemAlias(c *check.C) {
	d := s.daemon(c)
	s.mockSnap(c, `
name: core
version: 1
`)
	// Mock the hook runner
	hookRunner := testutil.MockCommand(c, "snap", "")
	defer hookRunner.Restore()

	d.overlord.Loop()
	defer d.overlord.Stop()

	text, err := json.Marshal(map[string]interface{}{"proxy.ftp": "value"})
	c.Assert(err, check.IsNil)

	buffer := bytes.NewBuffer(text)
	req, err := http.NewRequest("PUT", "/v2/snaps/system/conf", buffer)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "system"}

	rec := httptest.NewRecorder()
	snapConfCmd.PUT(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	tr := config.NewTransaction(st)
	st.Unlock()
	c.Assert(err, check.IsNil)

	var value string
	tr.Get("core", "proxy.ftp", &value)
	c.Assert(value, check.Equals, "value")

}

func (s *apiSuite) TestSetConfNumber(c *check.C) {
	d := s.daemon(c)
	s.mockSnap(c, configYaml)

	// Mock the hook runner
	hookRunner := testutil.MockCommand(c, "snap", "")
	defer hookRunner.Restore()

	d.overlord.Loop()
	defer d.overlord.Stop()

	text, err := json.Marshal(map[string]interface{}{"key": 1234567890})
	c.Assert(err, check.IsNil)

	buffer := bytes.NewBuffer(text)
	req, err := http.NewRequest("PUT", "/v2/snaps/config-snap/conf", buffer)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "config-snap"}

	rec := httptest.NewRecorder()
	snapConfCmd.PUT(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	tr := config.NewTransaction(d.overlord.State())
	var result interface{}
	c.Assert(tr.Get("config-snap", "key", &result), check.IsNil)
	c.Assert(result, check.DeepEquals, json.Number("1234567890"))
}

func (s *apiSuite) TestSetConfBadSnap(c *check.C) {
	s.daemonWithOverlordMock(c)

	text, err := json.Marshal(map[string]interface{}{"key": "value"})
	c.Assert(err, check.IsNil)

	buffer := bytes.NewBuffer(text)
	req, err := http.NewRequest("PUT", "/v2/snaps/config-snap/conf", buffer)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "config-snap"}

	rec := httptest.NewRecorder()
	snapConfCmd.PUT(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 404)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"status-code": 404.,
		"status":      "Not Found",
		"result": map[string]interface{}{
			"message": `snap "config-snap" is not installed`,
			"kind":    "snap-not-found",
			"value":   "config-snap",
		},
		"type": "error"})
}

func simulateConflict(o *overlord.Overlord, name string) {
	st := o.State()
	st.Lock()
	defer st.Unlock()
	t := st.NewTask("link-snap", "...")
	snapsup := &snapstate.SnapSetup{SideInfo: &snap.SideInfo{
		RealName: name,
	}}
	t.Set("snap-setup", snapsup)
	chg := st.NewChange("manip", "...")
	chg.AddTask(t)
}

func (s *apiSuite) TestSetConfChangeConflict(c *check.C) {
	d := s.daemon(c)
	s.mockSnap(c, configYaml)

	simulateConflict(d.overlord, "config-snap")

	text, err := json.Marshal(map[string]interface{}{"key": "value"})
	c.Assert(err, check.IsNil)

	buffer := bytes.NewBuffer(text)
	req, err := http.NewRequest("PUT", "/v2/snaps/config-snap/conf", buffer)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "config-snap"}

	rec := httptest.NewRecorder()
	snapConfCmd.PUT(snapConfCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 409)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Assert(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"status-code": 409.,
		"status":      "Conflict",
		"result": map[string]interface{}{
			"message": `snap "config-snap" has "manip" change in progress`,
			"kind":    "snap-change-conflict",
			"value": map[string]interface{}{
				"change-kind": "manip",
				"snap-name":   "config-snap",
			},
		},
		"type": "error"})
}

func (s *apiSuite) TestAppIconGet(c *check.C) {
	d := s.daemon(c)

	// have an active foo in the system
	info := s.mkInstalledInState(c, d, "foo", "bar", "v1", snap.R(10), true, "")

	// have an icon for it in the package itself
	iconfile := filepath.Join(info.MountDir(), "meta", "gui", "icon.ick")
	c.Assert(os.MkdirAll(filepath.Dir(iconfile), 0755), check.IsNil)
	c.Check(ioutil.WriteFile(iconfile, []byte("ick"), 0644), check.IsNil)

	s.vars = map[string]string{"name": "foo"}
	req, err := http.NewRequest("GET", "/v2/icons/foo/icon", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()

	appIconCmd.GET(appIconCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.Body.String(), check.Equals, "ick")
}

func (s *apiSuite) TestAppIconGetInactive(c *check.C) {
	d := s.daemon(c)

	// have an *in*active foo in the system
	info := s.mkInstalledInState(c, d, "foo", "bar", "v1", snap.R(10), false, "")

	// have an icon for it in the package itself
	iconfile := filepath.Join(info.MountDir(), "meta", "gui", "icon.ick")
	c.Assert(os.MkdirAll(filepath.Dir(iconfile), 0755), check.IsNil)
	c.Check(ioutil.WriteFile(iconfile, []byte("ick"), 0644), check.IsNil)

	s.vars = map[string]string{"name": "foo"}
	req, err := http.NewRequest("GET", "/v2/icons/foo/icon", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()

	appIconCmd.GET(appIconCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.Body.String(), check.Equals, "ick")
}

func (s *apiSuite) TestAppIconGetNoIcon(c *check.C) {
	d := s.daemon(c)

	// have an *in*active foo in the system
	info := s.mkInstalledInState(c, d, "foo", "bar", "v1", snap.R(10), true, "")

	// NO ICON!
	err := os.RemoveAll(filepath.Join(info.MountDir(), "meta", "gui", "icon.svg"))
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "foo"}
	req, err := http.NewRequest("GET", "/v2/icons/foo/icon", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()

	appIconCmd.GET(appIconCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code/100, check.Equals, 4)
}

func (s *apiSuite) TestAppIconGetNoApp(c *check.C) {
	s.daemon(c)

	s.vars = map[string]string{"name": "foo"}
	req, err := http.NewRequest("GET", "/v2/icons/foo/icon", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()

	appIconCmd.GET(appIconCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 404)
}

func (s *apiSuite) TestNotInstalledSnapIcon(c *check.C) {
	info := &snap.Info{SuggestedName: "notInstalledSnap", Media: []snap.MediaInfo{{Type: "icon", URL: "icon.svg"}}}
	iconfile := snapIcon(info)
	c.Check(iconfile, check.Equals, "")
}

func (s *apiSuite) TestInstallOnNonDevModeDistro(c *check.C) {
	s.testInstall(c, false, snapstate.Flags{}, snap.R(0))
}
func (s *apiSuite) TestInstallOnDevModeDistro(c *check.C) {
	s.testInstall(c, true, snapstate.Flags{}, snap.R(0))
}
func (s *apiSuite) TestInstallRevision(c *check.C) {
	s.testInstall(c, false, snapstate.Flags{}, snap.R(42))
}

func (s *apiSuite) testInstall(c *check.C, forcedDevmode bool, flags snapstate.Flags, revision snap.Revision) {
	calledFlags := snapstate.Flags{}
	installQueue := []string{}
	restore := release.MockForcedDevmode(forcedDevmode)
	defer restore()

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags
		installQueue = append(installQueue, name)
		c.Check(revision, check.Equals, opts.Revision)

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	defer func() {
		snapstateInstall = nil
	}()

	d := s.daemonWithFakeSnapManager(c)

	var buf bytes.Buffer
	if revision.Unset() {
		buf.WriteString(`{"action": "install"}`)
	} else {
		fmt.Fprintf(&buf, `{"action": "install", "revision": %s}`, revision.String())
	}
	req, err := http.NewRequest("POST", "/v2/snaps/some-snap", &buf)
	c.Assert(err, check.IsNil)

	s.vars = map[string]string{"name": "some-snap"}
	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Assert(rsp.Type, check.Equals, ResponseTypeAsync)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)

	c.Check(chg.Tasks(), check.HasLen, 1)

	st.Unlock()
	s.waitTrivialChange(c, chg)
	st.Lock()

	c.Check(chg.Status(), check.Equals, state.DoneStatus)
	c.Check(calledFlags, check.Equals, flags)
	c.Check(err, check.IsNil)
	c.Check(installQueue, check.DeepEquals, []string{"some-snap"})
	c.Check(chg.Kind(), check.Equals, "install-snap")
	c.Check(chg.Summary(), check.Equals, `Install "some-snap" snap`)
}

func (s *apiSuite) TestInstallUserAgentContextCreated(c *check.C) {
	snapstateInstall = func(ctx context.Context, st *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		s.ctx = ctx
		t := st.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	defer func() {
		snapstateInstall = nil
	}()

	s.daemonWithFakeSnapManager(c)

	var buf bytes.Buffer
	buf.WriteString(`{"action": "install"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/some-snap", &buf)
	req.RemoteAddr = "pid=100;uid=0;socket=;"
	c.Assert(err, check.IsNil)
	req.Header.Add("User-Agent", "some-agent/1.0")

	s.vars = map[string]string{"name": "some-snap"}
	rec := httptest.NewRecorder()
	snapCmd.ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	c.Check(store.ClientUserAgent(s.ctx), check.Equals, "some-agent/1.0")
}

func (s *apiSuite) TestRefresh(c *check.C) {
	var calledFlags snapstate.Flags
	calledUserID := 0
	installQueue := []string{}
	assertstateCalledUserID := 0

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags
		calledUserID = userID
		installQueue = append(installQueue, name)

		t := s.NewTask("fake-refresh-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		assertstateCalledUserID = userID
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "refresh",
		Snaps:  []string{"some-snap"},
		userID: 17,
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(assertstateCalledUserID, check.Equals, 17)
	c.Check(calledFlags, check.DeepEquals, snapstate.Flags{})
	c.Check(calledUserID, check.Equals, 17)
	c.Check(err, check.IsNil)
	c.Check(installQueue, check.DeepEquals, []string{"some-snap"})
	c.Check(summary, check.Equals, `Refresh "some-snap" snap`)
}

func (s *apiSuite) TestRefreshDevMode(c *check.C) {
	var calledFlags snapstate.Flags
	calledUserID := 0
	installQueue := []string{}

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags
		calledUserID = userID
		installQueue = append(installQueue, name)

		t := s.NewTask("fake-refresh-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:  "refresh",
		DevMode: true,
		Snaps:   []string{"some-snap"},
		userID:  17,
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	flags := snapstate.Flags{}
	flags.DevMode = true
	c.Check(calledFlags, check.DeepEquals, flags)
	c.Check(calledUserID, check.Equals, 17)
	c.Check(err, check.IsNil)
	c.Check(installQueue, check.DeepEquals, []string{"some-snap"})
	c.Check(summary, check.Equals, `Refresh "some-snap" snap`)
}

func (s *apiSuite) TestRefreshClassic(c *check.C) {
	var calledFlags snapstate.Flags

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags
		return nil, nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:  "refresh",
		Classic: true,
		Snaps:   []string{"some-snap"},
		userID:  17,
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(calledFlags, check.DeepEquals, snapstate.Flags{Classic: true})
}

func (s *apiSuite) TestRefreshIgnoreValidation(c *check.C) {
	var calledFlags snapstate.Flags
	calledUserID := 0
	installQueue := []string{}

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags
		calledUserID = userID
		installQueue = append(installQueue, name)

		t := s.NewTask("fake-refresh-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:           "refresh",
		IgnoreValidation: true,
		Snaps:            []string{"some-snap"},
		userID:           17,
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	flags := snapstate.Flags{}
	flags.IgnoreValidation = true

	c.Check(calledFlags, check.DeepEquals, flags)
	c.Check(calledUserID, check.Equals, 17)
	c.Check(err, check.IsNil)
	c.Check(installQueue, check.DeepEquals, []string{"some-snap"})
	c.Check(summary, check.Equals, `Refresh "some-snap" snap`)
}

func (s *apiSuite) TestRefreshCohort(c *check.C) {
	cohort := ""

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		cohort = opts.CohortKey

		t := s.NewTask("fake-refresh-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "refresh",
		Snaps:  []string{"some-snap"},
		snapRevisionOptions: snapRevisionOptions{
			CohortKey: "xyzzy",
		},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(cohort, check.Equals, "xyzzy")
	c.Check(summary, check.Equals, `Refresh "some-snap" snap`)
}

func (s *apiSuite) TestRefreshLeaveCohort(c *check.C) {
	var leave *bool

	snapstateUpdate = func(s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		leave = &opts.LeaveCohort

		t := s.NewTask("fake-refresh-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		return nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:              "refresh",
		snapRevisionOptions: snapRevisionOptions{LeaveCohort: true},
		Snaps:               []string{"some-snap"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(*leave, check.Equals, true)
	c.Check(summary, check.Equals, `Refresh "some-snap" snap`)
}

func (s *apiSuite) TestSwitchInstruction(c *check.C) {
	var cohort, channel string
	var leave *bool
	snapstateSwitch = func(s *state.State, name string, opts *snapstate.RevisionOptions) (*state.TaskSet, error) {
		cohort = opts.CohortKey
		leave = &opts.LeaveCohort
		channel = opts.Channel

		t := s.NewTask("fake-switch", "Doing a fake switch")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	st := d.overlord.State()

	type T struct {
		channel string
		cohort  string
		leave   bool
		summary string
	}
	table := []T{
		{"", "some-cohort", false, `Switch "some-snap" snap to cohort "…me-cohort"`},
		{"some-channel", "", false, `Switch "some-snap" snap to channel "some-channel"`},
		{"some-channel", "some-cohort", false, `Switch "some-snap" snap to channel "some-channel" and cohort "…me-cohort"`},
		{"", "", true, `Switch "some-snap" snap away from cohort`},
		{"some-channel", "", true, `Switch "some-snap" snap to channel "some-channel" and away from cohort`},
	}

	for _, t := range table {
		cohort, channel = "", ""
		leave = nil
		inst := &snapInstruction{
			Action: "switch",
			snapRevisionOptions: snapRevisionOptions{
				CohortKey:   t.cohort,
				LeaveCohort: t.leave,
				Channel:     t.channel,
			},
			Snaps: []string{"some-snap"},
		}

		st.Lock()
		summary, _, err := inst.dispatch()(inst, st)
		st.Unlock()
		c.Check(err, check.IsNil)

		c.Check(cohort, check.Equals, t.cohort)
		c.Check(channel, check.Equals, t.channel)
		c.Check(summary, check.Equals, t.summary)
		c.Check(*leave, check.Equals, t.leave)
	}
}

func (s *apiSuite) TestPostSnapsOp(c *check.C) {
	assertstateRefreshSnapDeclarations = func(*state.State, int) error { return nil }
	snapstateUpdateMany = func(_ context.Context, s *state.State, names []string, userID int, flags *snapstate.Flags) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 0)
		t := s.NewTask("fake-refresh-all", "Refreshing everything")
		return []string{"fake1", "fake2"}, []*state.TaskSet{state.NewTaskSet(t)}, nil
	}

	d := s.daemonWithOverlordMock(c)

	buf := bytes.NewBufferString(`{"action": "refresh"}`)
	req, err := http.NewRequest("POST", "/v2/login", buf)
	c.Assert(err, check.IsNil)
	req.Header.Set("Content-Type", "application/json")

	rsp, ok := postSnaps(snapsCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)
	c.Check(rsp.Type, check.Equals, ResponseTypeAsync)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Check(chg.Summary(), check.Equals, `Refresh snaps "fake1", "fake2"`)
	var apiData map[string]interface{}
	c.Check(chg.Get("api-data", &apiData), check.IsNil)
	c.Check(apiData["snap-names"], check.DeepEquals, []interface{}{"fake1", "fake2"})
}

func (s *apiSuite) TestRefreshAll(c *check.C) {
	refreshSnapDecls := false
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		refreshSnapDecls = true
		return assertstate.RefreshSnapDeclarations(s, userID)
	}
	d := s.daemon(c)

	for _, tst := range []struct {
		snaps []string
		msg   string
	}{
		{nil, "Refresh all snaps: no updates"},
		{[]string{"fake"}, `Refresh snap "fake"`},
		{[]string{"fake1", "fake2"}, `Refresh snaps "fake1", "fake2"`},
	} {
		refreshSnapDecls = false

		snapstateUpdateMany = func(_ context.Context, s *state.State, names []string, userID int, flags *snapstate.Flags) ([]string, []*state.TaskSet, error) {
			c.Check(names, check.HasLen, 0)
			t := s.NewTask("fake-refresh-all", "Refreshing everything")
			return tst.snaps, []*state.TaskSet{state.NewTaskSet(t)}, nil
		}

		inst := &snapInstruction{Action: "refresh"}
		st := d.overlord.State()
		st.Lock()
		res, err := snapUpdateMany(inst, st)
		st.Unlock()
		c.Assert(err, check.IsNil)
		c.Check(res.Summary, check.Equals, tst.msg)
		c.Check(refreshSnapDecls, check.Equals, true)
	}
}

func (s *apiSuite) TestRefreshAllNoChanges(c *check.C) {
	refreshSnapDecls := false
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		refreshSnapDecls = true
		return assertstate.RefreshSnapDeclarations(s, userID)
	}

	snapstateUpdateMany = func(_ context.Context, s *state.State, names []string, userID int, flags *snapstate.Flags) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 0)
		return nil, nil, nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{Action: "refresh"}
	st := d.overlord.State()
	st.Lock()
	res, err := snapUpdateMany(inst, st)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Check(res.Summary, check.Equals, `Refresh all snaps: no updates`)
	c.Check(refreshSnapDecls, check.Equals, true)
}

func (s *apiSuite) TestRefreshMany(c *check.C) {
	refreshSnapDecls := false
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		refreshSnapDecls = true
		return nil
	}

	snapstateUpdateMany = func(_ context.Context, s *state.State, names []string, userID int, flags *snapstate.Flags) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 2)
		t := s.NewTask("fake-refresh-2", "Refreshing two")
		return names, []*state.TaskSet{state.NewTaskSet(t)}, nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{Action: "refresh", Snaps: []string{"foo", "bar"}}
	st := d.overlord.State()
	st.Lock()
	res, err := snapUpdateMany(inst, st)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Check(res.Summary, check.Equals, `Refresh snaps "foo", "bar"`)
	c.Check(res.Affected, check.DeepEquals, inst.Snaps)
	c.Check(refreshSnapDecls, check.Equals, true)
}

func (s *apiSuite) TestRefreshMany1(c *check.C) {
	refreshSnapDecls := false
	assertstateRefreshSnapDeclarations = func(s *state.State, userID int) error {
		refreshSnapDecls = true
		return nil
	}

	snapstateUpdateMany = func(_ context.Context, s *state.State, names []string, userID int, flags *snapstate.Flags) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 1)
		t := s.NewTask("fake-refresh-1", "Refreshing one")
		return names, []*state.TaskSet{state.NewTaskSet(t)}, nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{Action: "refresh", Snaps: []string{"foo"}}
	st := d.overlord.State()
	st.Lock()
	res, err := snapUpdateMany(inst, st)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Check(res.Summary, check.Equals, `Refresh snap "foo"`)
	c.Check(res.Affected, check.DeepEquals, inst.Snaps)
	c.Check(refreshSnapDecls, check.Equals, true)
}

func (s *apiSuite) TestInstallMany(c *check.C) {
	snapstateInstallMany = func(s *state.State, names []string, userID int) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 2)
		t := s.NewTask("fake-install-2", "Install two")
		return names, []*state.TaskSet{state.NewTaskSet(t)}, nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{Action: "install", Snaps: []string{"foo", "bar"}}
	st := d.overlord.State()
	st.Lock()
	res, err := snapInstallMany(inst, st)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Check(res.Summary, check.Equals, `Install snaps "foo", "bar"`)
	c.Check(res.Affected, check.DeepEquals, inst.Snaps)
}

func (s *apiSuite) TestInstallManyEmptyName(c *check.C) {
	snapstateInstallMany = func(_ *state.State, _ []string, _ int) ([]string, []*state.TaskSet, error) {
		return nil, nil, errors.New("should not be called")
	}
	d := s.daemon(c)
	inst := &snapInstruction{Action: "install", Snaps: []string{"", "bar"}}
	st := d.overlord.State()
	st.Lock()
	res, err := snapInstallMany(inst, st)
	st.Unlock()
	c.Assert(res, check.IsNil)
	c.Assert(err, check.ErrorMatches, "cannot install snap with empty name")
}

func (s *apiSuite) TestRemoveMany(c *check.C) {
	snapstateRemoveMany = func(s *state.State, names []string) ([]string, []*state.TaskSet, error) {
		c.Check(names, check.HasLen, 2)
		t := s.NewTask("fake-remove-2", "Remove two")
		return names, []*state.TaskSet{state.NewTaskSet(t)}, nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{Action: "remove", Snaps: []string{"foo", "bar"}}
	st := d.overlord.State()
	st.Lock()
	res, err := snapRemoveMany(inst, st)
	st.Unlock()
	c.Assert(err, check.IsNil)
	c.Check(res.Summary, check.Equals, `Remove snaps "foo", "bar"`)
	c.Check(res.Affected, check.DeepEquals, inst.Snaps)
}

func (s *apiSuite) TestInstallFails(c *check.C) {
	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		t := s.NewTask("fake-install-snap-error", "Install task")
		return state.NewTaskSet(t), nil
	}

	d := s.daemonWithFakeSnapManager(c)
	s.vars = map[string]string{"name": "hello-world"}
	buf := bytes.NewBufferString(`{"action": "install"}`)
	req, err := http.NewRequest("POST", "/v2/snaps/hello-world", buf)
	c.Assert(err, check.IsNil)

	rsp := postSnap(snapCmd, req, nil).(*resp)

	c.Assert(rsp.Type, check.Equals, ResponseTypeAsync)

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)

	c.Check(chg.Tasks(), check.HasLen, 1)

	st.Unlock()
	s.waitTrivialChange(c, chg)
	st.Lock()

	c.Check(chg.Err(), check.ErrorMatches, `(?sm).*Install task \(fake-install-snap-error errored\)`)
}

func (s *apiSuite) TestInstallLeaveOld(c *check.C) {
	c.Skip("temporarily dropped half-baked support while sorting out flag mess")
	var calledFlags snapstate.Flags

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:   "install",
		LeaveOld: true,
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Assert(err, check.IsNil)

	c.Check(calledFlags, check.DeepEquals, snapstate.Flags{})
	c.Check(err, check.IsNil)
}

func (s *apiSuite) TestInstall(c *check.C) {
	var calledName string

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledName = name

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "install",
		// Install the snap in developer mode
		DevMode: true,
		Snaps:   []string{"fake"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)
	c.Check(calledName, check.Equals, "fake")
}

func (s *apiSuite) TestInstallCohort(c *check.C) {
	var calledName string
	var calledCohort string

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledName = name
		calledCohort = opts.CohortKey

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "install",
		snapRevisionOptions: snapRevisionOptions{
			CohortKey: "To the legion of the lost ones, to the cohort of the damned.",
		},
		Snaps: []string{"fake"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	msg, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)
	c.Check(calledName, check.Equals, "fake")
	c.Check(calledCohort, check.Equals, "To the legion of the lost ones, to the cohort of the damned.")
	c.Check(msg, check.Equals, `Install "fake" snap from "…e damned." cohort`)
}

func (s *apiSuite) TestInstallDevMode(c *check.C) {
	var calledFlags snapstate.Flags

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "install",
		// Install the snap in developer mode
		DevMode: true,
		Snaps:   []string{"fake"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(calledFlags.DevMode, check.Equals, true)
}

func (s *apiSuite) TestInstallJailMode(c *check.C) {
	var calledFlags snapstate.Flags

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:   "install",
		JailMode: true,
		Snaps:    []string{"fake"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(calledFlags.JailMode, check.Equals, true)
}

func (s *apiSuite) TestInstallJailModeDevModeOS(c *check.C) {
	restore := release.MockForcedDevmode(true)
	defer restore()

	d := s.daemon(c)
	inst := &snapInstruction{
		Action:   "install",
		JailMode: true,
		Snaps:    []string{"foo"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.ErrorMatches, "this system cannot honour the jailmode flag")
}

func (s *apiSuite) TestInstallEmptyName(c *check.C) {
	snapstateInstall = func(ctx context.Context, _ *state.State, _ string, _ *snapstate.RevisionOptions, _ int, _ snapstate.Flags) (*state.TaskSet, error) {
		return nil, errors.New("should not be called")
	}
	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "install",
		Snaps:  []string{""},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.ErrorMatches, "cannot install snap with empty name")
}

func (s *apiSuite) TestInstallJailModeDevMode(c *check.C) {
	d := s.daemon(c)
	inst := &snapInstruction{
		Action:   "install",
		DevMode:  true,
		JailMode: true,
		Snaps:    []string{"foo"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.ErrorMatches, "cannot use devmode and jailmode flags together")
}

func (s *apiSuite) testRevertSnap(inst *snapInstruction, c *check.C) {
	queue := []string{}

	instFlags, err := inst.modeFlags()
	c.Assert(err, check.IsNil)

	snapstateRevert = func(s *state.State, name string, flags snapstate.Flags) (*state.TaskSet, error) {
		c.Check(flags, check.Equals, instFlags)
		queue = append(queue, name)
		return nil, nil
	}
	snapstateRevertToRevision = func(s *state.State, name string, rev snap.Revision, flags snapstate.Flags) (*state.TaskSet, error) {
		c.Check(flags, check.Equals, instFlags)
		queue = append(queue, fmt.Sprintf("%s (%s)", name, rev))
		return nil, nil
	}

	d := s.daemon(c)
	inst.Action = "revert"
	inst.Snaps = []string{"some-snap"}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	summary, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)
	if inst.Revision.Unset() {
		c.Check(queue, check.DeepEquals, []string{inst.Snaps[0]})
	} else {
		c.Check(queue, check.DeepEquals, []string{fmt.Sprintf("%s (%s)", inst.Snaps[0], inst.Revision)})
	}
	c.Check(summary, check.Equals, `Revert "some-snap" snap`)
}

func (s *apiSuite) TestRevertSnap(c *check.C) {
	s.testRevertSnap(&snapInstruction{}, c)
}

func (s *apiSuite) TestRevertSnapDevMode(c *check.C) {
	s.testRevertSnap(&snapInstruction{DevMode: true}, c)
}

func (s *apiSuite) TestRevertSnapJailMode(c *check.C) {
	s.testRevertSnap(&snapInstruction{JailMode: true}, c)
}

func (s *apiSuite) TestRevertSnapClassic(c *check.C) {
	s.testRevertSnap(&snapInstruction{Classic: true}, c)
}

func (s *apiSuite) TestRevertSnapToRevision(c *check.C) {
	s.testRevertSnap(&snapInstruction{snapRevisionOptions: snapRevisionOptions{Revision: snap.R(1)}}, c)
}

func (s *apiSuite) TestRevertSnapToRevisionDevMode(c *check.C) {
	s.testRevertSnap(&snapInstruction{snapRevisionOptions: snapRevisionOptions{Revision: snap.R(1)}, DevMode: true}, c)
}

func (s *apiSuite) TestRevertSnapToRevisionJailMode(c *check.C) {
	s.testRevertSnap(&snapInstruction{snapRevisionOptions: snapRevisionOptions{Revision: snap.R(1)}, JailMode: true}, c)
}

func (s *apiSuite) TestRevertSnapToRevisionClassic(c *check.C) {
	s.testRevertSnap(&snapInstruction{snapRevisionOptions: snapRevisionOptions{Revision: snap.R(1)}, Classic: true}, c)
}

func snapList(rawSnaps interface{}) []map[string]interface{} {
	snaps := make([]map[string]interface{}, len(rawSnaps.([]*json.RawMessage)))
	for i, raw := range rawSnaps.([]*json.RawMessage) {
		err := json.Unmarshal([]byte(*raw), &snaps[i])
		if err != nil {
			panic(err)
		}
	}
	return snaps
}

// inverseCaseMapper implements SnapMapper to use lower case internally and upper case externally.
type inverseCaseMapper struct {
	ifacestate.IdentityMapper // Embed the identity mapper to reuse empty state mapping functions.
}

func (m *inverseCaseMapper) RemapSnapFromRequest(snapName string) string {
	return strings.ToLower(snapName)
}

func (m *inverseCaseMapper) RemapSnapToResponse(snapName string) string {
	return strings.ToUpper(snapName)
}

func (m *inverseCaseMapper) SystemSnapName() string {
	return "core"
}

// Tests for GET /v2/interfaces

func (s *apiSuite) TestInterfacesLegacy(c *check.C) {
	restore := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer restore()
	// Install an inverse case mapper to exercise the interface mapping at the same time.
	restore = ifacestate.MockSnapMapper(&inverseCaseMapper{})
	defer restore()

	d := s.daemon(c)

	var anotherConsumerYaml = `
name: another-consumer-%s
version: 1
apps:
 app:
plugs:
 plug:
  interface: test
  key: value
  label: label
`
	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, fmt.Sprintf(anotherConsumerYaml, "def"))
	s.mockSnap(c, fmt.Sprintf(anotherConsumerYaml, "abc"))
	s.mockSnap(c, producerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}
	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)

	st := s.d.overlord.State()
	st.Lock()
	st.Set("conns", map[string]interface{}{
		"consumer:plug producer:slot": map[string]interface{}{
			"interface": "test",
			"auto":      true,
		},
		"another-consumer-def:plug producer:slot": map[string]interface{}{
			"interface": "test",
			"by-gadget": true,
			"auto":      true,
		},
		"another-consumer-abc:plug producer:slot": map[string]interface{}{
			"interface": "test",
			"by-gadget": true,
			"auto":      true,
		},
	})
	st.Unlock()

	req, err := http.NewRequest("GET", "/v2/interfaces", nil)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.GET(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 200)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"plugs": []interface{}{
				map[string]interface{}{
					"snap":      "another-consumer-abc",
					"plug":      "plug",
					"interface": "test",
					"attrs":     map[string]interface{}{"key": "value"},
					"apps":      []interface{}{"app"},
					"label":     "label",
					"connections": []interface{}{
						map[string]interface{}{"snap": "producer", "slot": "slot"},
					},
				},
				map[string]interface{}{
					"snap":      "another-consumer-def",
					"plug":      "plug",
					"interface": "test",
					"attrs":     map[string]interface{}{"key": "value"},
					"apps":      []interface{}{"app"},
					"label":     "label",
					"connections": []interface{}{
						map[string]interface{}{"snap": "producer", "slot": "slot"},
					},
				},
				map[string]interface{}{
					"snap":      "consumer",
					"plug":      "plug",
					"interface": "test",
					"attrs":     map[string]interface{}{"key": "value"},
					"apps":      []interface{}{"app"},
					"label":     "label",
					"connections": []interface{}{
						map[string]interface{}{"snap": "producer", "slot": "slot"},
					},
				},
			},
			"slots": []interface{}{
				map[string]interface{}{
					"snap":      "producer",
					"slot":      "slot",
					"interface": "test",
					"attrs":     map[string]interface{}{"key": "value"},
					"apps":      []interface{}{"app"},
					"label":     "label",
					"connections": []interface{}{
						map[string]interface{}{"snap": "another-consumer-abc", "plug": "plug"},
						map[string]interface{}{"snap": "another-consumer-def", "plug": "plug"},
						map[string]interface{}{"snap": "consumer", "plug": "plug"},
					},
				},
			},
		},
		"status":      "OK",
		"status-code": 200.0,
		"type":        "sync",
	})
}

func (s *apiSuite) TestInterfacesModern(c *check.C) {
	restore := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer restore()
	// Install an inverse case mapper to exercise the interface mapping at the same time.
	restore = ifacestate.MockSnapMapper(&inverseCaseMapper{})
	defer restore()

	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}
	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("GET", "/v2/interfaces?select=connected&doc=true&plugs=true&slots=true", nil)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.GET(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 200)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": []interface{}{
			map[string]interface{}{
				"name": "test",
				"plugs": []interface{}{
					map[string]interface{}{
						"snap":  "consumer",
						"plug":  "plug",
						"label": "label",
						"attrs": map[string]interface{}{
							"key": "value",
						},
					}},
				"slots": []interface{}{
					map[string]interface{}{
						"snap":  "producer",
						"slot":  "slot",
						"label": "label",
						"attrs": map[string]interface{}{
							"key": "value",
						},
					},
				},
			},
		},
		"status":      "OK",
		"status-code": 200.0,
		"type":        "sync",
	})
}

// Test for POST /v2/interfaces

func (s *apiSuite) TestConnectPlugSuccess(c *check.C) {
	restore := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer restore()
	// Install an inverse case mapper to exercise the interface mapping at the same time.
	restore = ifacestate.MockSnapMapper(&inverseCaseMapper{})
	defer restore()

	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "CONSUMER", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "PRODUCER", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	repo := d.overlord.InterfaceManager().Repository()
	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 1)
	c.Check(ifaces.Connections, check.DeepEquals, []*interfaces.ConnRef{{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}})
}

func (s *apiSuite) TestConnectPlugFailureInterfaceMismatch(c *check.C) {
	d := s.daemon(c)

	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "test"})
	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "different"})
	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, differentProducerYaml)

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "cannot connect consumer:plug (\"test\" interface) to producer:slot (\"different\" interface)",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
	repo := d.overlord.InterfaceManager().Repository()
	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 0)
}

func (s *apiSuite) TestConnectPlugFailureNoSuchPlug(c *check.C) {
	d := s.daemon(c)

	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "test"})
	// there is no consumer, no plug defined
	s.mockSnap(c, producerYaml)
	s.mockSnap(c, consumerYaml)

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "missingplug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "snap \"consumer\" has no plug named \"missingplug\"",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})

	repo := d.overlord.InterfaceManager().Repository()
	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 0)
}

func (s *apiSuite) TestConnectAlreadyConnected(c *check.C) {
	d := s.daemon(c)

	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "test"})
	// there is no consumer, no plug defined
	s.mockSnap(c, producerYaml)
	s.mockSnap(c, consumerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}

	d.overlord.Loop()
	defer d.overlord.Stop()

	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)
	conns := map[string]interface{}{
		"consumer:plug producer:slot": map[string]interface{}{
			"auto": false,
		},
	}
	st := d.overlord.State()
	st.Lock()
	st.Set("conns", conns)
	st.Unlock()

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st.Lock()
	chg := st.Change(id)
	c.Assert(chg.Tasks(), check.HasLen, 0)
	c.Assert(chg.Status(), check.Equals, state.DoneStatus)
	st.Unlock()
}

func (s *apiSuite) TestConnectPlugFailureNoSuchSlot(c *check.C) {
	d := s.daemon(c)

	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "test"})
	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)
	// there is no producer, no slot defined

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "missingslot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "snap \"producer\" has no slot named \"missingslot\"",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})

	repo := d.overlord.InterfaceManager().Repository()
	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 0)
}

func (s *apiSuite) TestConnectPlugChangeConflict(c *check.C) {
	d := s.daemon(c)

	s.mockIface(c, &ifacetest.TestInterface{InterfaceName: "test"})
	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)
	// there is no producer, no slot defined

	simulateConflict(d.overlord, "consumer")

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 409)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"status-code": 409.,
		"status":      "Conflict",
		"result": map[string]interface{}{
			"message": `snap "consumer" has "manip" change in progress`,
			"kind":    "snap-change-conflict",
			"value": map[string]interface{}{
				"change-kind": "manip",
				"snap-name":   "consumer",
			},
		},
		"type": "error"})
}

func (s *apiSuite) TestConnectCoreSystemAlias(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, coreProducerYaml)

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &interfaceAction{
		Action: "connect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "system", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	repo := d.overlord.InterfaceManager().Repository()
	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 1)
	c.Check(ifaces.Connections, check.DeepEquals, []*interfaces.ConnRef{{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "core", Name: "slot"}}})
}

func (s *apiSuite) testDisconnect(c *check.C, plugSnap, plugName, slotSnap, slotName string) {
	restore := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer restore()
	// Install an inverse case mapper to exercise the interface mapping at the same time.
	restore = ifacestate.MockSnapMapper(&inverseCaseMapper{})
	defer restore()
	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}
	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)

	st := d.overlord.State()
	st.Lock()
	st.Set("conns", map[string]interface{}{
		"consumer:plug producer:slot": map[string]interface{}{
			"interface": "test",
		},
	})
	st.Unlock()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: plugSnap, Name: plugName}},
		Slots:  []slotJSON{{Snap: slotSnap, Name: slotName}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 0)
}

func (s *apiSuite) TestDisconnectPlugSuccess(c *check.C) {
	s.testDisconnect(c, "CONSUMER", "plug", "PRODUCER", "slot")
}

func (s *apiSuite) TestDisconnectPlugSuccessWithEmptyPlug(c *check.C) {
	s.testDisconnect(c, "", "", "PRODUCER", "slot")
}

func (s *apiSuite) TestDisconnectPlugSuccessWithEmptySlot(c *check.C) {
	s.testDisconnect(c, "CONSUMER", "plug", "", "")
}

func (s *apiSuite) TestDisconnectPlugFailureNoSuchPlug(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	s.daemon(c)

	// there is no consumer, no plug defined
	s.mockSnap(c, producerYaml)

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "snap \"consumer\" has no plug named \"plug\"",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestDisconnectPlugNothingToDo(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "", Name: ""}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "nothing to do",
			"kind":    "interfaces-unchanged",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestDisconnectPlugFailureNoSuchSlot(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	s.daemon(c)

	s.mockSnap(c, consumerYaml)
	// there is no producer, no slot defined

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)

	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "snap \"producer\" has no slot named \"slot\"",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestDisconnectPlugFailureNotConnected(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)

	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "cannot disconnect consumer:plug from producer:slot, it is not connected",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestDisconnectConflict(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, producerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "producer", Name: "slot"},
	}
	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)

	st := d.overlord.State()
	st.Lock()
	st.Set("conns", map[string]interface{}{
		"consumer:plug producer:slot": map[string]interface{}{
			"interface": "test",
		},
	})
	st.Unlock()

	simulateConflict(d.overlord, "consumer")

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "producer", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)

	c.Check(rec.Code, check.Equals, 409)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"status-code": 409.,
		"status":      "Conflict",
		"result": map[string]interface{}{
			"message": `snap "consumer" has "manip" change in progress`,
			"kind":    "snap-change-conflict",
			"value": map[string]interface{}{
				"change-kind": "manip",
				"snap-name":   "consumer",
			},
		},
		"type": "error"})
}

func (s *apiSuite) TestDisconnectCoreSystemAlias(c *check.C) {
	revert := builtin.MockInterface(&ifacetest.TestInterface{InterfaceName: "test"})
	defer revert()
	d := s.daemon(c)

	s.mockSnap(c, consumerYaml)
	s.mockSnap(c, coreProducerYaml)

	repo := d.overlord.InterfaceManager().Repository()
	connRef := &interfaces.ConnRef{
		PlugRef: interfaces.PlugRef{Snap: "consumer", Name: "plug"},
		SlotRef: interfaces.SlotRef{Snap: "core", Name: "slot"},
	}
	_, err := repo.Connect(connRef, nil, nil, nil, nil, nil)
	c.Assert(err, check.IsNil)

	st := d.overlord.State()
	st.Lock()
	st.Set("conns", map[string]interface{}{
		"consumer:plug core:slot": map[string]interface{}{
			"interface": "test",
		},
	})
	st.Unlock()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &interfaceAction{
		Action: "disconnect",
		Plugs:  []plugJSON{{Snap: "consumer", Name: "plug"}},
		Slots:  []slotJSON{{Snap: "system", Name: "slot"}},
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	ifaces := repo.Interfaces()
	c.Assert(ifaces.Connections, check.HasLen, 0)
}

func (s *apiSuite) TestUnsupportedInterfaceRequest(c *check.C) {
	buf := bytes.NewBuffer([]byte(`garbage`))
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "cannot decode request body into an interface action: invalid character 'g' looking for beginning of value",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestMissingInterfaceAction(c *check.C) {
	action := &interfaceAction{}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "interface action not specified",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func (s *apiSuite) TestUnsupportedInterfaceAction(c *check.C) {
	s.daemon(c)
	action := &interfaceAction{Action: "foo"}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/interfaces", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	interfacesCmd.POST(interfacesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 400)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"result": map[string]interface{}{
			"message": "unsupported interface action: \"foo\"",
		},
		"status":      "Bad Request",
		"status-code": 400.0,
		"type":        "error",
	})
}

func setupChanges(st *state.State) []string {
	chg1 := st.NewChange("install", "install...")
	chg1.Set("snap-names", []string{"funky-snap-name"})
	t1 := st.NewTask("download", "1...")
	t2 := st.NewTask("activate", "2...")
	chg1.AddAll(state.NewTaskSet(t1, t2))
	t1.Logf("l11")
	t1.Logf("l12")
	chg2 := st.NewChange("remove", "remove..")
	t3 := st.NewTask("unlink", "1...")
	chg2.AddTask(t3)
	t3.SetStatus(state.ErrorStatus)
	t3.Errorf("rm failed")

	return []string{chg1.ID(), chg2.ID(), t1.ID(), t2.ID(), t3.ID()}
}

func (s *apiSuite) TestStateChangesDefaultToInProgress(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	setupChanges(st)
	st.Unlock()

	// Execute
	req, err := http.NewRequest("GET", "/v2/changes", nil)
	c.Assert(err, check.IsNil)
	rsp := getChanges(stateChangesCmd, req, nil).(*resp)

	// Verify
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.HasLen, 1)

	res, err := rsp.MarshalJSON()
	c.Assert(err, check.IsNil)

	c.Check(string(res), check.Matches, `.*{"id":"\w+","kind":"install","summary":"install...","status":"Do","tasks":\[{"id":"\w+","kind":"download","summary":"1...","status":"Do","log":\["2016-04-21T01:02:03Z INFO l11","2016-04-21T01:02:03Z INFO l12"],"progress":{"label":"","done":0,"total":1},"spawn-time":"2016-04-21T01:02:03Z"}.*`)
}

func (s *apiSuite) TestStateChangesInProgress(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	setupChanges(st)
	st.Unlock()

	// Execute
	req, err := http.NewRequest("GET", "/v2/changes?select=in-progress", nil)
	c.Assert(err, check.IsNil)
	rsp := getChanges(stateChangesCmd, req, nil).(*resp)

	// Verify
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.HasLen, 1)

	res, err := rsp.MarshalJSON()
	c.Assert(err, check.IsNil)

	c.Check(string(res), check.Matches, `.*{"id":"\w+","kind":"install","summary":"install...","status":"Do","tasks":\[{"id":"\w+","kind":"download","summary":"1...","status":"Do","log":\["2016-04-21T01:02:03Z INFO l11","2016-04-21T01:02:03Z INFO l12"],"progress":{"label":"","done":0,"total":1},"spawn-time":"2016-04-21T01:02:03Z"}.*],"ready":false,"spawn-time":"2016-04-21T01:02:03Z"}.*`)
}

func (s *apiSuite) TestStateChangesAll(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	setupChanges(st)
	st.Unlock()

	// Execute
	req, err := http.NewRequest("GET", "/v2/changes?select=all", nil)
	c.Assert(err, check.IsNil)
	rsp := getChanges(stateChangesCmd, req, nil).(*resp)

	// Verify
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.HasLen, 2)

	res, err := rsp.MarshalJSON()
	c.Assert(err, check.IsNil)

	c.Check(string(res), check.Matches, `.*{"id":"\w+","kind":"install","summary":"install...","status":"Do","tasks":\[{"id":"\w+","kind":"download","summary":"1...","status":"Do","log":\["2016-04-21T01:02:03Z INFO l11","2016-04-21T01:02:03Z INFO l12"],"progress":{"label":"","done":0,"total":1},"spawn-time":"2016-04-21T01:02:03Z"}.*],"ready":false,"spawn-time":"2016-04-21T01:02:03Z"}.*`)
	c.Check(string(res), check.Matches, `.*{"id":"\w+","kind":"remove","summary":"remove..","status":"Error","tasks":\[{"id":"\w+","kind":"unlink","summary":"1...","status":"Error","log":\["2016-04-21T01:02:03Z ERROR rm failed"],"progress":{"label":"","done":1,"total":1},"spawn-time":"2016-04-21T01:02:03Z","ready-time":"2016-04-21T01:02:03Z"}.*],"ready":true,"err":"[^"]+".*`)
}

func (s *apiSuite) TestStateChangesReady(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	setupChanges(st)
	st.Unlock()

	// Execute
	req, err := http.NewRequest("GET", "/v2/changes?select=ready", nil)
	c.Assert(err, check.IsNil)
	rsp := getChanges(stateChangesCmd, req, nil).(*resp)

	// Verify
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.HasLen, 1)

	res, err := rsp.MarshalJSON()
	c.Assert(err, check.IsNil)

	c.Check(string(res), check.Matches, `.*{"id":"\w+","kind":"remove","summary":"remove..","status":"Error","tasks":\[{"id":"\w+","kind":"unlink","summary":"1...","status":"Error","log":\["2016-04-21T01:02:03Z ERROR rm failed"],"progress":{"label":"","done":1,"total":1},"spawn-time":"2016-04-21T01:02:03Z","ready-time":"2016-04-21T01:02:03Z"}.*],"ready":true,"err":"[^"]+".*`)
}

func (s *apiSuite) TestStateChangesForSnapName(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	setupChanges(st)
	st.Unlock()

	// Execute
	req, err := http.NewRequest("GET", "/v2/changes?for=funky-snap-name&select=all", nil)
	c.Assert(err, check.IsNil)
	rsp := getChanges(stateChangesCmd, req, nil).(*resp)

	// Verify
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.FitsTypeOf, []*changeInfo(nil))

	res := rsp.Result.([]*changeInfo)
	c.Assert(res, check.HasLen, 1)
	c.Check(res[0].Kind, check.Equals, `install`)

	_, err = rsp.MarshalJSON()
	c.Assert(err, check.IsNil)
}

func (s *apiSuite) TestStateChange(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	ids := setupChanges(st)
	chg := st.Change(ids[0])
	chg.Set("api-data", map[string]int{"n": 42})
	st.Unlock()
	s.vars = map[string]string{"id": ids[0]}

	// Execute
	req, err := http.NewRequest("POST", "/v2/change/"+ids[0], nil)
	c.Assert(err, check.IsNil)
	rsp := getChange(stateChangeCmd, req, nil).(*resp)
	rec := httptest.NewRecorder()
	rsp.ServeHTTP(rec, req)

	// Verify
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Result, check.NotNil)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body["result"], check.DeepEquals, map[string]interface{}{
		"id":         ids[0],
		"kind":       "install",
		"summary":    "install...",
		"status":     "Do",
		"ready":      false,
		"spawn-time": "2016-04-21T01:02:03Z",
		"tasks": []interface{}{
			map[string]interface{}{
				"id":         ids[2],
				"kind":       "download",
				"summary":    "1...",
				"status":     "Do",
				"log":        []interface{}{"2016-04-21T01:02:03Z INFO l11", "2016-04-21T01:02:03Z INFO l12"},
				"progress":   map[string]interface{}{"label": "", "done": 0., "total": 1.},
				"spawn-time": "2016-04-21T01:02:03Z",
			},
			map[string]interface{}{
				"id":         ids[3],
				"kind":       "activate",
				"summary":    "2...",
				"status":     "Do",
				"progress":   map[string]interface{}{"label": "", "done": 0., "total": 1.},
				"spawn-time": "2016-04-21T01:02:03Z",
			},
		},
		"data": map[string]interface{}{
			"n": float64(42),
		},
	})
}

func (s *apiSuite) TestStateChangeAbort(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	soon := 0
	ensureStateSoon = func(st *state.State) {
		soon++
	}

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	ids := setupChanges(st)
	st.Unlock()
	s.vars = map[string]string{"id": ids[0]}

	buf := bytes.NewBufferString(`{"action": "abort"}`)

	// Execute
	req, err := http.NewRequest("POST", "/v2/changes/"+ids[0], buf)
	c.Assert(err, check.IsNil)
	rsp := abortChange(stateChangeCmd, req, nil).(*resp)
	rec := httptest.NewRecorder()
	rsp.ServeHTTP(rec, req)

	// Ensure scheduled
	c.Check(soon, check.Equals, 1)

	// Verify
	c.Check(rec.Code, check.Equals, 200)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Result, check.NotNil)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body["result"], check.DeepEquals, map[string]interface{}{
		"id":         ids[0],
		"kind":       "install",
		"summary":    "install...",
		"status":     "Hold",
		"ready":      true,
		"spawn-time": "2016-04-21T01:02:03Z",
		"ready-time": "2016-04-21T01:02:03Z",
		"tasks": []interface{}{
			map[string]interface{}{
				"id":         ids[2],
				"kind":       "download",
				"summary":    "1...",
				"status":     "Hold",
				"log":        []interface{}{"2016-04-21T01:02:03Z INFO l11", "2016-04-21T01:02:03Z INFO l12"},
				"progress":   map[string]interface{}{"label": "", "done": 1., "total": 1.},
				"spawn-time": "2016-04-21T01:02:03Z",
				"ready-time": "2016-04-21T01:02:03Z",
			},
			map[string]interface{}{
				"id":         ids[3],
				"kind":       "activate",
				"summary":    "2...",
				"status":     "Hold",
				"progress":   map[string]interface{}{"label": "", "done": 1., "total": 1.},
				"spawn-time": "2016-04-21T01:02:03Z",
				"ready-time": "2016-04-21T01:02:03Z",
			},
		},
	})
}

func (s *apiSuite) TestStateChangeAbortIsReady(c *check.C) {
	restore := state.MockTime(time.Date(2016, 04, 21, 1, 2, 3, 0, time.UTC))
	defer restore()

	// Setup
	d := newTestDaemon(c)
	st := d.overlord.State()
	st.Lock()
	ids := setupChanges(st)
	st.Change(ids[0]).SetStatus(state.DoneStatus)
	st.Unlock()
	s.vars = map[string]string{"id": ids[0]}

	buf := bytes.NewBufferString(`{"action": "abort"}`)

	// Execute
	req, err := http.NewRequest("POST", "/v2/changes/"+ids[0], buf)
	c.Assert(err, check.IsNil)
	rsp := abortChange(stateChangeCmd, req, nil).(*resp)
	rec := httptest.NewRecorder()
	rsp.ServeHTTP(rec, req)

	// Verify
	c.Check(rec.Code, check.Equals, 400)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result, check.NotNil)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body["result"], check.DeepEquals, map[string]interface{}{
		"message": fmt.Sprintf("cannot abort change %s with nothing pending", ids[0]),
	})
}

const validBuyInput = `{
		  "snap-id": "the-snap-id-1234abcd",
		  "snap-name": "the snap name",
		  "price": 1.23,
		  "currency": "EUR"
		}`

var validBuyOptions = &client.BuyOptions{
	SnapID:   "the-snap-id-1234abcd",
	Price:    1.23,
	Currency: "EUR",
}

var buyTests = []struct {
	input                string
	result               *client.BuyResult
	err                  error
	expectedStatus       int
	expectedResult       interface{}
	expectedResponseType ResponseType
	expectedBuyOptions   *client.BuyOptions
}{
	{
		// Success
		input: validBuyInput,
		result: &client.BuyResult{
			State: "Complete",
		},
		expectedStatus: 200,
		expectedResult: &client.BuyResult{
			State: "Complete",
		},
		expectedResponseType: ResponseTypeSync,
		expectedBuyOptions:   validBuyOptions,
	},
	{
		// Fail with internal error
		input: `{
		  "snap-id": "the-snap-id-1234abcd",
		  "price": 1.23,
		  "currency": "EUR"
		}`,
		err:                  fmt.Errorf("internal error banana"),
		expectedStatus:       500,
		expectedResponseType: ResponseTypeError,
		expectedResult: &errorResult{
			Message: "internal error banana",
		},
		expectedBuyOptions: &client.BuyOptions{
			SnapID:   "the-snap-id-1234abcd",
			Price:    1.23,
			Currency: "EUR",
		},
	},
	{
		// Fail with unauthenticated error
		input:                validBuyInput,
		err:                  store.ErrUnauthenticated,
		expectedStatus:       400,
		expectedResponseType: ResponseTypeError,
		expectedResult: &errorResult{
			Message: "you need to log in first",
			Kind:    "login-required",
		},
		expectedBuyOptions: validBuyOptions,
	},
	{
		// Fail with TOS not accepted
		input:                validBuyInput,
		err:                  store.ErrTOSNotAccepted,
		expectedStatus:       400,
		expectedResponseType: ResponseTypeError,
		expectedResult: &errorResult{
			Message: "terms of service not accepted",
			Kind:    "terms-not-accepted",
		},
		expectedBuyOptions: validBuyOptions,
	},
	{
		// Fail with no payment methods
		input:                validBuyInput,
		err:                  store.ErrNoPaymentMethods,
		expectedStatus:       400,
		expectedResponseType: ResponseTypeError,
		expectedResult: &errorResult{
			Message: "no payment methods",
			Kind:    "no-payment-methods",
		},
		expectedBuyOptions: validBuyOptions,
	},
	{
		// Fail with payment declined
		input:                validBuyInput,
		err:                  store.ErrPaymentDeclined,
		expectedStatus:       400,
		expectedResponseType: ResponseTypeError,
		expectedResult: &errorResult{
			Message: "payment declined",
			Kind:    "payment-declined",
		},
		expectedBuyOptions: validBuyOptions,
	},
}

func (s *apiSuite) TestBuySnap(c *check.C) {
	s.daemon(c)

	for _, test := range buyTests {
		s.buyResult = test.result
		s.err = test.err

		buf := bytes.NewBufferString(test.input)
		req, err := http.NewRequest("POST", "/v2/buy", buf)
		c.Assert(err, check.IsNil)

		state := snapCmd.d.overlord.State()
		state.Lock()
		user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
		state.Unlock()
		c.Check(err, check.IsNil)

		rsp := postBuy(buyCmd, req, user).(*resp)

		c.Check(rsp.Status, check.Equals, test.expectedStatus)
		c.Check(rsp.Type, check.Equals, test.expectedResponseType)
		c.Assert(rsp.Result, check.FitsTypeOf, test.expectedResult)
		c.Check(rsp.Result, check.DeepEquals, test.expectedResult)

		c.Check(s.buyOptions, check.DeepEquals, test.expectedBuyOptions)
		c.Check(s.user, check.Equals, user)
	}
}

func (s *apiSuite) TestIsTrue(c *check.C) {
	form := &multipart.Form{}
	c.Check(isTrue(form, "foo"), check.Equals, false)
	for _, f := range []string{"", "false", "0", "False", "f", "try"} {
		form.Value = map[string][]string{"foo": {f}}
		c.Check(isTrue(form, "foo"), check.Equals, false, check.Commentf("expected %q to be false", f))
	}
	for _, t := range []string{"true", "1", "True", "t"} {
		form.Value = map[string][]string{"foo": {t}}
		c.Check(isTrue(form, "foo"), check.Equals, true, check.Commentf("expected %q to be true", t))
	}
}

var readyToBuyTests = []struct {
	input    error
	status   int
	respType interface{}
	response interface{}
}{
	{
		// Success
		input:    nil,
		status:   200,
		respType: ResponseTypeSync,
		response: true,
	},
	{
		// Not accepted TOS
		input:    store.ErrTOSNotAccepted,
		status:   400,
		respType: ResponseTypeError,
		response: &errorResult{
			Message: "terms of service not accepted",
			Kind:    errorKindTermsNotAccepted,
		},
	},
	{
		// No payment methods
		input:    store.ErrNoPaymentMethods,
		status:   400,
		respType: ResponseTypeError,
		response: &errorResult{
			Message: "no payment methods",
			Kind:    errorKindNoPaymentMethods,
		},
	},
}

func (s *apiSuite) TestReadyToBuy(c *check.C) {
	s.daemon(c)

	for _, test := range readyToBuyTests {
		s.err = test.input

		req, err := http.NewRequest("GET", "/v2/buy/ready", nil)
		c.Assert(err, check.IsNil)

		state := snapCmd.d.overlord.State()
		state.Lock()
		user, err := auth.NewUser(state, "username", "email@test.com", "macaroon", []string{"discharge"})
		state.Unlock()
		c.Check(err, check.IsNil)

		rsp := readyToBuy(readyToBuyCmd, req, user).(*resp)
		c.Check(rsp.Status, check.Equals, test.status)
		c.Check(rsp.Type, check.Equals, test.respType)
		c.Assert(rsp.Result, check.FitsTypeOf, test.response)
		c.Check(rsp.Result, check.DeepEquals, test.response)
	}
}

// aliases

func (s *apiSuite) TestAliasSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "alias",
		Snap:   "alias-snap",
		App:    "app",
		Alias:  "alias1",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	// sanity check
	c.Check(osutil.IsSymlink(filepath.Join(dirs.SnapBinariesDir, "alias1")), check.Equals, true)
}

func (s *apiSuite) TestAliasChangeConflict(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	simulateConflict(d.overlord, "alias-snap")

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	action := &aliasAction{
		Action: "alias",
		Snap:   "alias-snap",
		App:    "app",
		Alias:  "alias1",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Check(rec.Code, check.Equals, 409)

	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	c.Check(body, check.DeepEquals, map[string]interface{}{
		"status-code": 409.,
		"status":      "Conflict",
		"result": map[string]interface{}{
			"message": `snap "alias-snap" has "manip" change in progress`,
			"kind":    "snap-change-conflict",
			"value": map[string]interface{}{
				"change-kind": "manip",
				"snap-name":   "alias-snap",
			},
		},
		"type": "error"})
}

func (s *apiSuite) TestAliasErrors(c *check.C) {
	s.daemon(c)

	errScenarios := []struct {
		mangle func(*aliasAction)
		err    string
	}{
		{func(a *aliasAction) { a.Action = "" }, `unsupported alias action: ""`},
		{func(a *aliasAction) { a.Action = "what" }, `unsupported alias action: "what"`},
		{func(a *aliasAction) { a.Snap = "lalala" }, `snap "lalala" is not installed`},
		{func(a *aliasAction) { a.Alias = ".foo" }, `invalid alias name: ".foo"`},
		{func(a *aliasAction) { a.Aliases = []string{"baz"} }, `cannot interpret request, snaps can no longer be expected to declare their aliases`},
	}

	for _, scen := range errScenarios {
		action := &aliasAction{
			Action: "alias",
			Snap:   "alias-snap",
			App:    "app",
			Alias:  "alias1",
		}
		scen.mangle(action)

		text, err := json.Marshal(action)
		c.Assert(err, check.IsNil)
		buf := bytes.NewBuffer(text)
		req, err := http.NewRequest("POST", "/v2/aliases", buf)
		c.Assert(err, check.IsNil)

		rsp := changeAliases(aliasesCmd, req, nil).(*resp)
		c.Check(rsp.Type, check.Equals, ResponseTypeError)
		c.Check(rsp.Status, check.Equals, 400)
		c.Check(rsp.Result.(*errorResult).Message, check.Matches, scen.err)
	}
}

func (s *apiSuite) TestUnaliasSnapSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "unalias",
		Snap:   "alias-snap",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	c.Check(chg.Summary(), check.Equals, `Disable all aliases for snap "alias-snap"`)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	// sanity check
	var snapst snapstate.SnapState
	err = snapstate.Get(st, "alias-snap", &snapst)
	c.Assert(err, check.IsNil)
	c.Check(snapst.AutoAliasesDisabled, check.Equals, true)
}

func (s *apiSuite) TestUnaliasDWIMSnapSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "unalias",
		Snap:   "alias-snap",
		Alias:  "alias-snap",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	c.Check(chg.Summary(), check.Equals, `Disable all aliases for snap "alias-snap"`)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	// sanity check
	var snapst snapstate.SnapState
	err = snapstate.Get(st, "alias-snap", &snapst)
	c.Assert(err, check.IsNil)
	c.Check(snapst.AutoAliasesDisabled, check.Equals, true)
}

func (s *apiSuite) TestUnaliasAliasSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "alias",
		Snap:   "alias-snap",
		App:    "app",
		Alias:  "alias1",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	// unalias
	action = &aliasAction{
		Action: "unalias",
		Alias:  "alias1",
	}
	text, err = json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf = bytes.NewBuffer(text)
	req, err = http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec = httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id = body["change"].(string)

	st.Lock()
	chg = st.Change(id)
	c.Check(chg.Summary(), check.Equals, `Remove manual alias "alias1" for snap "alias-snap"`)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	// sanity check
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapBinariesDir, "alias1")), check.Equals, false)
}

func (s *apiSuite) TestUnaliasDWIMAliasSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "alias",
		Snap:   "alias-snap",
		App:    "app",
		Alias:  "alias1",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	err = chg.Err()
	st.Unlock()
	c.Assert(err, check.IsNil)

	// DWIM unalias an alias
	action = &aliasAction{
		Action: "unalias",
		Snap:   "alias1",
		Alias:  "alias1",
	}
	text, err = json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf = bytes.NewBuffer(text)
	req, err = http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec = httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id = body["change"].(string)

	st.Lock()
	chg = st.Change(id)
	c.Check(chg.Summary(), check.Equals, `Remove manual alias "alias1" for snap "alias-snap"`)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	// sanity check
	c.Check(osutil.FileExists(filepath.Join(dirs.SnapBinariesDir, "alias1")), check.Equals, false)
}

func (s *apiSuite) TestPreferSuccess(c *check.C) {
	err := os.MkdirAll(dirs.SnapBinariesDir, 0755)
	c.Assert(err, check.IsNil)
	d := s.daemon(c)

	s.mockSnap(c, aliasYaml)

	oldAutoAliases := snapstate.AutoAliases
	snapstate.AutoAliases = func(*state.State, *snap.Info) (map[string]string, error) {
		return nil, nil
	}
	defer func() { snapstate.AutoAliases = oldAutoAliases }()

	d.overlord.Loop()
	defer d.overlord.Stop()

	action := &aliasAction{
		Action: "prefer",
		Snap:   "alias-snap",
	}
	text, err := json.Marshal(action)
	c.Assert(err, check.IsNil)
	buf := bytes.NewBuffer(text)
	req, err := http.NewRequest("POST", "/v2/aliases", buf)
	c.Assert(err, check.IsNil)
	rec := httptest.NewRecorder()
	aliasesCmd.POST(aliasesCmd, req, nil).ServeHTTP(rec, req)
	c.Assert(rec.Code, check.Equals, 202)
	var body map[string]interface{}
	err = json.Unmarshal(rec.Body.Bytes(), &body)
	c.Check(err, check.IsNil)
	id := body["change"].(string)

	st := d.overlord.State()
	st.Lock()
	chg := st.Change(id)
	c.Check(chg.Summary(), check.Equals, `Prefer aliases of snap "alias-snap"`)
	st.Unlock()
	c.Assert(chg, check.NotNil)

	<-chg.Ready()

	st.Lock()
	defer st.Unlock()
	err = chg.Err()
	c.Assert(err, check.IsNil)

	// sanity check
	var snapst snapstate.SnapState
	err = snapstate.Get(st, "alias-snap", &snapst)
	c.Assert(err, check.IsNil)
	c.Check(snapst.AutoAliasesDisabled, check.Equals, false)
}

func (s *apiSuite) TestAliases(c *check.C) {
	d := s.daemon(c)

	st := d.overlord.State()
	st.Lock()
	snapstate.Set(st, "alias-snap1", &snapstate.SnapState{
		Sequence: []*snap.SideInfo{
			{RealName: "alias-snap1", Revision: snap.R(11)},
		},
		Current: snap.R(11),
		Active:  true,
		Aliases: map[string]*snapstate.AliasTarget{
			"alias1": {Manual: "cmd1x", Auto: "cmd1"},
			"alias2": {Auto: "cmd2"},
		},
	})
	snapstate.Set(st, "alias-snap2", &snapstate.SnapState{
		Sequence: []*snap.SideInfo{
			{RealName: "alias-snap2", Revision: snap.R(12)},
		},
		Current:             snap.R(12),
		Active:              true,
		AutoAliasesDisabled: true,
		Aliases: map[string]*snapstate.AliasTarget{
			"alias2": {Auto: "cmd2"},
			"alias3": {Manual: "cmd3"},
			"alias4": {Manual: "cmd4x", Auto: "cmd4"},
		},
	})
	st.Unlock()

	req, err := http.NewRequest("GET", "/v2/aliases", nil)
	c.Assert(err, check.IsNil)

	rsp := getAliases(aliasesCmd, req, nil).(*resp)
	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Check(rsp.Result, check.DeepEquals, map[string]map[string]aliasStatus{
		"alias-snap1": {
			"alias1": {
				Command: "alias-snap1.cmd1x",
				Status:  "manual",
				Manual:  "cmd1x",
				Auto:    "cmd1",
			},
			"alias2": {
				Command: "alias-snap1.cmd2",
				Status:  "auto",
				Auto:    "cmd2",
			},
		},
		"alias-snap2": {
			"alias2": {
				Command: "alias-snap2.cmd2",
				Status:  "disabled",
				Auto:    "cmd2",
			},
			"alias3": {
				Command: "alias-snap2.cmd3",
				Status:  "manual",
				Manual:  "cmd3",
			},
			"alias4": {
				Command: "alias-snap2.cmd4x",
				Status:  "manual",
				Manual:  "cmd4x",
				Auto:    "cmd4",
			},
		},
	})

}

func (s *apiSuite) TestInstallUnaliased(c *check.C) {
	var calledFlags snapstate.Flags

	snapstateInstall = func(ctx context.Context, s *state.State, name string, opts *snapstate.RevisionOptions, userID int, flags snapstate.Flags) (*state.TaskSet, error) {
		calledFlags = flags

		t := s.NewTask("fake-install-snap", "Doing a fake install")
		return state.NewTaskSet(t), nil
	}

	d := s.daemon(c)
	inst := &snapInstruction{
		Action: "install",
		// Install the snap without enabled automatic aliases
		Unaliased: true,
		Snaps:     []string{"fake"},
	}

	st := d.overlord.State()
	st.Lock()
	defer st.Unlock()
	_, _, err := inst.dispatch()(inst, st)
	c.Check(err, check.IsNil)

	c.Check(calledFlags.Unaliased, check.Equals, true)
}

func (s *apiSuite) TestInstallPathUnaliased(c *check.C) {
	body := "" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"snap\"; filename=\"x\"\r\n" +
		"\r\n" +
		"xyzzy\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"devmode\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n" +
		"Content-Disposition: form-data; name=\"unaliased\"\r\n" +
		"\r\n" +
		"true\r\n" +
		"----hello--\r\n"
	head := map[string]string{"Content-Type": "multipart/thing; boundary=--hello--"}
	// try a multipart/form-data upload
	flags := snapstate.Flags{Unaliased: true, RemoveSnapPath: true, DevMode: true}
	chgSummary := s.sideloadCheck(c, body, head, "local", flags)
	c.Check(chgSummary, check.Equals, `Install "local" snap from file "x"`)
}

func (s *apiSuite) TestSnapctlGetNoUID(c *check.C) {
	buf := bytes.NewBufferString(`{"context-id": "some-context", "args": ["get", "something"]}`)
	req, err := http.NewRequest("POST", "/v2/snapctl", buf)
	c.Assert(err, check.IsNil)
	rsp := runSnapctl(snapctlCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 403)
}

func (s *apiSuite) TestSnapctlForbiddenError(c *check.C) {
	_ = s.daemon(c)

	runSnapctlUcrednetGet = func(string) (int32, uint32, string, error) {
		return 100, 9999, dirs.SnapSocket, nil
	}
	defer func() { runSnapctlUcrednetGet = ucrednetGet }()
	ctlcmdRun = func(ctx *hookstate.Context, arg []string, uid uint32) ([]byte, []byte, error) {
		return nil, nil, &ctlcmd.ForbiddenCommandError{}
	}
	defer func() { ctlcmdRun = ctlcmd.Run }()

	buf := bytes.NewBufferString(fmt.Sprintf(`{"context-id": "some-context", "args": [%q, %q]}`, "set", "foo=bar"))
	req, err := http.NewRequest("POST", "/v2/snapctl", buf)
	c.Assert(err, check.IsNil)
	rsp := runSnapctl(snapctlCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 403)
}

type appSuite struct {
	apiBaseSuite
	cmd *testutil.MockCmd

	infoA, infoB, infoC, infoD *snap.Info
}

var _ = check.Suite(&appSuite{})

func (s *appSuite) SetUpTest(c *check.C) {
	s.apiBaseSuite.SetUpTest(c)
	s.cmd = testutil.MockCommand(c, "systemctl", "").Also("journalctl", "")
	s.daemon(c)
	s.infoA = s.mkInstalledInState(c, s.d, "snap-a", "dev", "v1", snap.R(1), true, "apps: {svc1: {daemon: simple}, svc2: {daemon: simple, reload-command: x}}")
	s.infoB = s.mkInstalledInState(c, s.d, "snap-b", "dev", "v1", snap.R(1), false, "apps: {svc3: {daemon: simple}, cmd1: {}}")
	s.infoC = s.mkInstalledInState(c, s.d, "snap-c", "dev", "v1", snap.R(1), true, "")
	s.infoD = s.mkInstalledInState(c, s.d, "snap-d", "dev", "v1", snap.R(1), true, "apps: {cmd2: {}, cmd3: {}}")
	s.d.overlord.Loop()
}

func (s *appSuite) TearDownTest(c *check.C) {
	s.d.overlord.Stop()
	s.cmd.Restore()
	s.apiBaseSuite.TearDownTest(c)
}

func (s *appSuite) TestSplitAppName(c *check.C) {
	type T struct {
		name string
		snap string
		app  string
	}

	for _, x := range []T{
		{name: "foo.bar", snap: "foo", app: "bar"},
		{name: "foo", snap: "foo", app: ""},
		{name: "foo.bar.baz", snap: "foo", app: "bar.baz"},
		{name: ".", snap: "", app: ""}, // SISO
	} {
		snap, app := splitAppName(x.name)
		c.Check(x.snap, check.Equals, snap, check.Commentf(x.name))
		c.Check(x.app, check.Equals, app, check.Commentf(x.name))
	}
}

func (s *appSuite) TestGetAppsInfo(c *check.C) {
	svcNames := []string{"snap-a.svc1", "snap-a.svc2", "snap-b.svc3"}
	for _, name := range svcNames {
		s.sysctlBufs = append(s.sysctlBufs, []byte(fmt.Sprintf(`
Id=snap.%s.service
Type=simple
ActiveState=active
UnitFileState=enabled
`[1:], name)))
	}

	req, err := http.NewRequest("GET", "/v2/apps", nil)
	c.Assert(err, check.IsNil)

	rsp := getAppsInfo(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, []client.AppInfo{})
	apps := rsp.Result.([]client.AppInfo)
	c.Assert(apps, check.HasLen, 6)

	for _, name := range svcNames {
		snap, app := splitAppName(name)
		needle := client.AppInfo{
			Snap:   snap,
			Name:   app,
			Daemon: "simple",
		}
		if snap != "snap-b" {
			// snap-b is not active (all the others are)
			needle.Active = true
			needle.Enabled = true
		}
		c.Check(apps, testutil.DeepContains, needle)
	}

	for _, name := range []string{"snap-b.cmd1", "snap-d.cmd2", "snap-d.cmd3"} {
		snap, app := splitAppName(name)
		c.Check(apps, testutil.DeepContains, client.AppInfo{
			Snap: snap,
			Name: app,
		})
	}

	appNames := make([]string, len(apps))
	for i, app := range apps {
		appNames[i] = app.Snap + "." + app.Name
	}
	c.Check(sort.StringsAreSorted(appNames), check.Equals, true)
}

func (s *appSuite) TestGetAppsInfoNames(c *check.C) {

	req, err := http.NewRequest("GET", "/v2/apps?names=snap-d", nil)
	c.Assert(err, check.IsNil)

	rsp := getAppsInfo(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, []client.AppInfo{})
	apps := rsp.Result.([]client.AppInfo)
	c.Assert(apps, check.HasLen, 2)

	for _, name := range []string{"snap-d.cmd2", "snap-d.cmd3"} {
		snap, app := splitAppName(name)
		c.Check(apps, testutil.DeepContains, client.AppInfo{
			Snap: snap,
			Name: app,
		})
	}

	appNames := make([]string, len(apps))
	for i, app := range apps {
		appNames[i] = app.Snap + "." + app.Name
	}
	c.Check(sort.StringsAreSorted(appNames), check.Equals, true)
}

func (s *appSuite) TestGetAppsInfoServices(c *check.C) {
	svcNames := []string{"snap-a.svc1", "snap-a.svc2", "snap-b.svc3"}
	for _, name := range svcNames {
		s.sysctlBufs = append(s.sysctlBufs, []byte(fmt.Sprintf(`
Id=snap.%s.service
Type=simple
ActiveState=active
UnitFileState=enabled
`[1:], name)))
	}

	req, err := http.NewRequest("GET", "/v2/apps?select=service", nil)
	c.Assert(err, check.IsNil)

	rsp := getAppsInfo(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Type, check.Equals, ResponseTypeSync)
	c.Assert(rsp.Result, check.FitsTypeOf, []client.AppInfo{})
	svcs := rsp.Result.([]client.AppInfo)
	c.Assert(svcs, check.HasLen, 3)

	for _, name := range svcNames {
		snap, app := splitAppName(name)
		needle := client.AppInfo{
			Snap:   snap,
			Name:   app,
			Daemon: "simple",
		}
		if snap != "snap-b" {
			// snap-b is not active (all the others are)
			needle.Active = true
			needle.Enabled = true
		}
		c.Check(svcs, testutil.DeepContains, needle)
	}

	appNames := make([]string, len(svcs))
	for i, svc := range svcs {
		appNames[i] = svc.Snap + "." + svc.Name
	}
	c.Check(sort.StringsAreSorted(appNames), check.Equals, true)
}

func (s *appSuite) TestGetAppsInfoBadSelect(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/apps?select=potato", nil)
	c.Assert(err, check.IsNil)

	rsp := getAppsInfo(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 400)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestGetAppsInfoBadName(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/apps?names=potato", nil)
	c.Assert(err, check.IsNil)

	rsp := getAppsInfo(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 404)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestAppInfosForOne(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-a.svc1"}, appInfoOptions{service: true})
	c.Assert(rsp, check.IsNil)
	c.Assert(appInfos, check.HasLen, 1)
	c.Check(appInfos[0].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[0].Name, check.Equals, "svc1")
}

func (s *appSuite) TestAppInfosForAll(c *check.C) {
	type T struct {
		opts  appInfoOptions
		snaps []*snap.Info
		names []string
	}

	for _, t := range []T{
		{
			opts:  appInfoOptions{service: true},
			names: []string{"svc1", "svc2", "svc3"},
			snaps: []*snap.Info{s.infoA, s.infoA, s.infoB},
		},
		{
			opts:  appInfoOptions{},
			names: []string{"svc1", "svc2", "cmd1", "svc3", "cmd2", "cmd3"},
			snaps: []*snap.Info{s.infoA, s.infoA, s.infoB, s.infoB, s.infoD, s.infoD},
		},
	} {
		c.Assert(len(t.names), check.Equals, len(t.snaps), check.Commentf("%s", t.opts))

		st := s.d.overlord.State()
		appInfos, rsp := appInfosFor(st, nil, t.opts)
		c.Assert(rsp, check.IsNil, check.Commentf("%s", t.opts))
		names := make([]string, len(appInfos))
		for i, appInfo := range appInfos {
			names[i] = appInfo.Name
		}
		c.Assert(names, check.DeepEquals, t.names, check.Commentf("%s", t.opts))

		for i := range appInfos {
			c.Check(appInfos[i].Snap, check.DeepEquals, t.snaps[i], check.Commentf("%s: %s", t.opts, t.names[i]))
		}
	}
}

func (s *appSuite) TestAppInfosForOneSnap(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-a"}, appInfoOptions{service: true})
	c.Assert(rsp, check.IsNil)
	c.Assert(appInfos, check.HasLen, 2)
	sort.Sort(cmd.BySnapApp(appInfos))

	c.Check(appInfos[0].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[0].Name, check.Equals, "svc1")
	c.Check(appInfos[1].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[1].Name, check.Equals, "svc2")
}

func (s *appSuite) TestAppInfosForMixedArgs(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-a", "snap-a.svc1"}, appInfoOptions{service: true})
	c.Assert(rsp, check.IsNil)
	c.Assert(appInfos, check.HasLen, 2)
	sort.Sort(cmd.BySnapApp(appInfos))

	c.Check(appInfos[0].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[0].Name, check.Equals, "svc1")
	c.Check(appInfos[1].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[1].Name, check.Equals, "svc2")
}

func (s *appSuite) TestAppInfosCleanupAndSorted(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{
		"snap-b.svc3",
		"snap-a.svc2",
		"snap-a.svc1",
		"snap-a.svc2",
		"snap-b.svc3",
		"snap-a.svc1",
		"snap-b",
		"snap-a",
	}, appInfoOptions{service: true})
	c.Assert(rsp, check.IsNil)
	c.Assert(appInfos, check.HasLen, 3)
	sort.Sort(cmd.BySnapApp(appInfos))

	c.Check(appInfos[0].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[0].Name, check.Equals, "svc1")
	c.Check(appInfos[1].Snap, check.DeepEquals, s.infoA)
	c.Check(appInfos[1].Name, check.Equals, "svc2")
	c.Check(appInfos[2].Snap, check.DeepEquals, s.infoB)
	c.Check(appInfos[2].Name, check.Equals, "svc3")
}

func (s *appSuite) TestAppInfosForAppless(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-c"}, appInfoOptions{service: true})
	c.Assert(rsp, check.FitsTypeOf, &resp{})
	c.Check(rsp.(*resp).Status, check.Equals, 404)
	c.Check(rsp.(*resp).Result.(*errorResult).Kind, check.Equals, errorKindAppNotFound)
	c.Assert(appInfos, check.IsNil)
}

func (s *appSuite) TestAppInfosForMissingApp(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-c.whatever"}, appInfoOptions{service: true})
	c.Assert(rsp, check.FitsTypeOf, &resp{})
	c.Check(rsp.(*resp).Status, check.Equals, 404)
	c.Check(rsp.(*resp).Result.(*errorResult).Kind, check.Equals, errorKindAppNotFound)
	c.Assert(appInfos, check.IsNil)
}

func (s *appSuite) TestAppInfosForMissingSnap(c *check.C) {
	st := s.d.overlord.State()
	appInfos, rsp := appInfosFor(st, []string{"snap-x"}, appInfoOptions{service: true})
	c.Assert(rsp, check.FitsTypeOf, &resp{})
	c.Check(rsp.(*resp).Status, check.Equals, 404)
	c.Check(rsp.(*resp).Result.(*errorResult).Kind, check.Equals, errorKindSnapNotFound)
	c.Assert(appInfos, check.IsNil)
}

func (s *apiSuite) TestLogsNoServices(c *check.C) {
	// NOTE this is *apiSuite, not *appSuite, so there are no
	// installed snaps with services

	cmd := testutil.MockCommand(c, "systemctl", "").Also("journalctl", "")
	defer cmd.Restore()
	s.daemon(c)
	s.d.overlord.Loop()
	defer s.d.overlord.Stop()

	req, err := http.NewRequest("GET", "/v2/logs", nil)
	c.Assert(err, check.IsNil)

	rsp := getLogs(logsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 404)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestLogs(c *check.C) {
	s.jctlRCs = []io.ReadCloser{ioutil.NopCloser(strings.NewReader(`
{"MESSAGE": "hello1", "SYSLOG_IDENTIFIER": "xyzzy", "_PID": "42", "__REALTIME_TIMESTAMP": "42"}
{"MESSAGE": "hello2", "SYSLOG_IDENTIFIER": "xyzzy", "_PID": "42", "__REALTIME_TIMESTAMP": "44"}
{"MESSAGE": "hello3", "SYSLOG_IDENTIFIER": "xyzzy", "_PID": "42", "__REALTIME_TIMESTAMP": "46"}
{"MESSAGE": "hello4", "SYSLOG_IDENTIFIER": "xyzzy", "_PID": "42", "__REALTIME_TIMESTAMP": "48"}
{"MESSAGE": "hello5", "SYSLOG_IDENTIFIER": "xyzzy", "_PID": "42", "__REALTIME_TIMESTAMP": "50"}
	`))}

	req, err := http.NewRequest("GET", "/v2/logs?names=snap-a.svc2&n=42&follow=false", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()
	getLogs(logsCmd, req, nil).ServeHTTP(rec, req)

	c.Check(s.jctlSvcses, check.DeepEquals, [][]string{{"snap.snap-a.svc2.service"}})
	c.Check(s.jctlNs, check.DeepEquals, []int{42})
	c.Check(s.jctlFollows, check.DeepEquals, []bool{false})

	c.Check(rec.Code, check.Equals, 200)
	c.Check(rec.HeaderMap.Get("Content-Type"), check.Equals, "application/json-seq")
	c.Check(rec.Body.String(), check.Equals, `
{"timestamp":"1970-01-01T00:00:00.000042Z","message":"hello1","sid":"xyzzy","pid":"42"}
{"timestamp":"1970-01-01T00:00:00.000044Z","message":"hello2","sid":"xyzzy","pid":"42"}
{"timestamp":"1970-01-01T00:00:00.000046Z","message":"hello3","sid":"xyzzy","pid":"42"}
{"timestamp":"1970-01-01T00:00:00.000048Z","message":"hello4","sid":"xyzzy","pid":"42"}
{"timestamp":"1970-01-01T00:00:00.00005Z","message":"hello5","sid":"xyzzy","pid":"42"}
`[1:])
}

func (s *appSuite) TestLogsN(c *check.C) {
	type T struct {
		in  string
		out int
	}

	for _, t := range []T{
		{in: "", out: 10},
		{in: "0", out: 0},
		{in: "-1", out: -1},
		{in: strconv.Itoa(math.MinInt32), out: math.MinInt32},
		{in: strconv.Itoa(math.MaxInt32), out: math.MaxInt32},
	} {

		s.jctlRCs = []io.ReadCloser{ioutil.NopCloser(strings.NewReader(""))}
		s.jctlNs = nil

		req, err := http.NewRequest("GET", "/v2/logs?n="+t.in, nil)
		c.Assert(err, check.IsNil)

		rec := httptest.NewRecorder()
		getLogs(logsCmd, req, nil).ServeHTTP(rec, req)

		c.Check(s.jctlNs, check.DeepEquals, []int{t.out})
	}
}

func (s *appSuite) TestLogsBadN(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/logs?n=hello", nil)
	c.Assert(err, check.IsNil)

	rsp := getLogs(logsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 400)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestLogsFollow(c *check.C) {
	s.jctlRCs = []io.ReadCloser{
		ioutil.NopCloser(strings.NewReader("")),
		ioutil.NopCloser(strings.NewReader("")),
		ioutil.NopCloser(strings.NewReader("")),
	}

	reqT, err := http.NewRequest("GET", "/v2/logs?follow=true", nil)
	c.Assert(err, check.IsNil)
	reqF, err := http.NewRequest("GET", "/v2/logs?follow=false", nil)
	c.Assert(err, check.IsNil)
	reqN, err := http.NewRequest("GET", "/v2/logs", nil)
	c.Assert(err, check.IsNil)

	rec := httptest.NewRecorder()
	getLogs(logsCmd, reqT, nil).ServeHTTP(rec, reqT)
	getLogs(logsCmd, reqF, nil).ServeHTTP(rec, reqF)
	getLogs(logsCmd, reqN, nil).ServeHTTP(rec, reqN)

	c.Check(s.jctlFollows, check.DeepEquals, []bool{true, false, false})
}

func (s *appSuite) TestLogsBadFollow(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/logs?follow=hello", nil)
	c.Assert(err, check.IsNil)

	rsp := getLogs(logsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 400)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestLogsBadName(c *check.C) {
	req, err := http.NewRequest("GET", "/v2/logs?names=hello", nil)
	c.Assert(err, check.IsNil)

	rsp := getLogs(logsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 404)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) TestLogsSad(c *check.C) {
	s.jctlErrs = []error{errors.New("potato")}
	req, err := http.NewRequest("GET", "/v2/logs", nil)
	c.Assert(err, check.IsNil)

	rsp := getLogs(logsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 500)
	c.Assert(rsp.Type, check.Equals, ResponseTypeError)
}

func (s *appSuite) testPostApps(c *check.C, inst servicestate.Instruction, systemctlCall [][]string) *state.Change {
	postBody, err := json.Marshal(inst)
	c.Assert(err, check.IsNil)

	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBuffer(postBody))
	c.Assert(err, check.IsNil)

	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Assert(rsp.Status, check.Equals, 202)
	c.Assert(rsp.Type, check.Equals, ResponseTypeAsync)
	c.Check(rsp.Change, check.Matches, `[0-9]+`)

	st := s.d.overlord.State()
	st.Lock()
	defer st.Unlock()
	chg := st.Change(rsp.Change)
	c.Assert(chg, check.NotNil)
	c.Check(chg.Tasks(), check.HasLen, len(systemctlCall))

	st.Unlock()
	<-chg.Ready()
	st.Lock()

	c.Check(s.cmd.Calls(), check.DeepEquals, systemctlCall)
	return chg
}

func (s *appSuite) TestPostAppsStartOne(c *check.C) {
	inst := servicestate.Instruction{Action: "start", Names: []string{"snap-a.svc2"}}
	expected := [][]string{{"systemctl", "start", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPostAppsStartTwo(c *check.C) {
	inst := servicestate.Instruction{Action: "start", Names: []string{"snap-a"}}
	expected := [][]string{{"systemctl", "start", "snap.snap-a.svc1.service", "snap.snap-a.svc2.service"}}
	chg := s.testPostApps(c, inst, expected)
	chg.State().Lock()
	defer chg.State().Unlock()
	// check the summary expands the snap into actual apps
	c.Check(chg.Summary(), check.Equals, "Running service command")
	c.Check(chg.Tasks()[0].Summary(), check.Equals, "start of [snap-a.svc1 snap-a.svc2]")
}

func (s *appSuite) TestPostAppsStartThree(c *check.C) {
	inst := servicestate.Instruction{Action: "start", Names: []string{"snap-a", "snap-b"}}
	expected := [][]string{{"systemctl", "start", "snap.snap-a.svc1.service", "snap.snap-a.svc2.service", "snap.snap-b.svc3.service"}}
	chg := s.testPostApps(c, inst, expected)
	// check the summary expands the snap into actual apps
	c.Check(chg.Summary(), check.Equals, "Running service command")
	chg.State().Lock()
	defer chg.State().Unlock()
	c.Check(chg.Tasks()[0].Summary(), check.Equals, "start of [snap-a.svc1 snap-a.svc2 snap-b.svc3]")
}

func (s *appSuite) TestPosetAppsStop(c *check.C) {
	inst := servicestate.Instruction{Action: "stop", Names: []string{"snap-a.svc2"}}
	expected := [][]string{{"systemctl", "stop", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPosetAppsRestart(c *check.C) {
	inst := servicestate.Instruction{Action: "restart", Names: []string{"snap-a.svc2"}}
	expected := [][]string{{"systemctl", "restart", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPosetAppsReload(c *check.C) {
	inst := servicestate.Instruction{Action: "restart", Names: []string{"snap-a.svc2"}}
	inst.Reload = true
	expected := [][]string{{"systemctl", "reload-or-restart", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPosetAppsEnableNow(c *check.C) {
	inst := servicestate.Instruction{Action: "start", Names: []string{"snap-a.svc2"}}
	inst.Enable = true
	expected := [][]string{{"systemctl", "enable", "snap.snap-a.svc2.service"}, {"systemctl", "start", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPosetAppsDisableNow(c *check.C) {
	inst := servicestate.Instruction{Action: "stop", Names: []string{"snap-a.svc2"}}
	inst.Disable = true
	expected := [][]string{{"systemctl", "disable", "snap.snap-a.svc2.service"}, {"systemctl", "stop", "snap.snap-a.svc2.service"}}
	s.testPostApps(c, inst, expected)
}

func (s *appSuite) TestPostAppsBadJSON(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`'junk`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Matches, ".*cannot decode request body.*")
}

func (s *appSuite) TestPostAppsBadOp(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`{"random": "json"}`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Matches, ".*cannot perform operation on services without a list of services.*")
}

func (s *appSuite) TestPostAppsBadSnap(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`{"action": "stop", "names": ["snap-c"]}`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 404)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `snap "snap-c" has no services`)
}

func (s *appSuite) TestPostAppsBadApp(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`{"action": "stop", "names": ["snap-a.what"]}`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 404)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `snap "snap-a" has no service "what"`)
}

func (s *appSuite) TestPostAppsBadAction(c *check.C) {
	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`{"action": "discombobulate", "names": ["snap-a.svc1"]}`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `unknown action "discombobulate"`)
}

func (s *appSuite) TestPostAppsConflict(c *check.C) {
	st := s.d.overlord.State()
	st.Lock()
	locked := true
	defer func() {
		if locked {
			st.Unlock()
		}
	}()

	ts, err := snapstate.Remove(st, "snap-a", snap.R(0), nil)
	c.Assert(err, check.IsNil)
	// need a change to make the tasks visible
	st.NewChange("enable", "...").AddAll(ts)
	st.Unlock()
	locked = false

	req, err := http.NewRequest("POST", "/v2/apps", bytes.NewBufferString(`{"action": "start", "names": ["snap-a.svc1"]}`))
	c.Assert(err, check.IsNil)
	rsp := postApps(appsCmd, req, nil).(*resp)
	c.Check(rsp.Status, check.Equals, 400)
	c.Check(rsp.Type, check.Equals, ResponseTypeError)
	c.Check(rsp.Result.(*errorResult).Message, check.Equals, `snap "snap-a" has "enable" change in progress`)
}

type fakeNetError struct {
	message   string
	timeout   bool
	temporary bool
}

func (e fakeNetError) Error() string   { return e.message }
func (e fakeNetError) Timeout() bool   { return e.timeout }
func (e fakeNetError) Temporary() bool { return e.temporary }

func (s *apiSuite) TestErrToResponseNoSnapsDoesNotPanic(c *check.C) {
	si := &snapInstruction{Action: "frobble"}
	errors := []error{
		store.ErrSnapNotFound,
		&store.RevisionNotAvailableError{},
		store.ErrNoUpdateAvailable,
		store.ErrLocalSnap,
		&snap.AlreadyInstalledError{Snap: "foo"},
		&snap.NotInstalledError{Snap: "foo"},
		&snapstate.SnapNeedsDevModeError{Snap: "foo"},
		&snapstate.SnapNeedsClassicError{Snap: "foo"},
		&snapstate.SnapNeedsClassicSystemError{Snap: "foo"},
		fakeNetError{message: "other"},
		fakeNetError{message: "timeout", timeout: true},
		fakeNetError{message: "temp", temporary: true},
		errors.New("some other error"),
	}

	for _, err := range errors {
		rsp := si.errToResponse(err)
		com := check.Commentf("%v", err)
		c.Check(rsp, check.NotNil, com)
		status := rsp.(*resp).Status
		c.Check(status/100 == 4 || status/100 == 5, check.Equals, true, com)
	}
}

func (s *apiSuite) TestErrToResponseForRevisionNotAvailable(c *check.C) {
	si := &snapInstruction{Action: "frobble", Snaps: []string{"foo"}}

	thisArch := arch.DpkgArchitecture()

	err := &store.RevisionNotAvailableError{
		Action:  "install",
		Channel: "stable",
		Releases: []channel.Channel{
			snaptest.MustParseChannel("beta", thisArch),
		},
	}
	rsp := si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 404,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: "no snap revision on specified channel",
			Kind:    errorKindSnapChannelNotAvailable,
			Value: map[string]interface{}{
				"snap-name":    "foo",
				"action":       "install",
				"channel":      "stable",
				"architecture": thisArch,
				"releases": []map[string]interface{}{
					{"architecture": thisArch, "channel": "beta"},
				},
			},
		},
	})

	err = &store.RevisionNotAvailableError{
		Action:  "install",
		Channel: "stable",
		Releases: []channel.Channel{
			snaptest.MustParseChannel("beta", "other-arch"),
		},
	}
	rsp = si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 404,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: "no snap revision on specified architecture",
			Kind:    errorKindSnapArchitectureNotAvailable,
			Value: map[string]interface{}{
				"snap-name":    "foo",
				"action":       "install",
				"channel":      "stable",
				"architecture": thisArch,
				"releases": []map[string]interface{}{
					{"architecture": "other-arch", "channel": "beta"},
				},
			},
		},
	})

	err = &store.RevisionNotAvailableError{}
	rsp = si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 404,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: "no snap revision available as specified",
			Kind:    errorKindSnapRevisionNotAvailable,
			Value:   "foo",
		},
	})
}

func (s *apiSuite) testWarnings(c *check.C, all bool, body io.Reader) (calls string, result interface{}) {
	s.daemon(c)

	oldOK := stateOkayWarnings
	oldAll := stateAllWarnings
	oldPending := statePendingWarnings
	stateOkayWarnings = func(*state.State, time.Time) int { calls += "ok"; return 0 }
	stateAllWarnings = func(*state.State) []*state.Warning { calls += "all"; return nil }
	statePendingWarnings = func(*state.State) ([]*state.Warning, time.Time) { calls += "show"; return nil, time.Time{} }
	defer func() {
		stateOkayWarnings = oldOK
		stateAllWarnings = oldAll
		statePendingWarnings = oldPending
	}()

	method := "GET"
	f := warningsCmd.GET
	if body != nil {
		method = "POST"
		f = warningsCmd.POST
	}
	q := url.Values{}
	if all {
		q.Set("select", "all")
	}
	req, err := http.NewRequest(method, "/v2/warnings?"+q.Encode(), body)
	c.Assert(err, check.IsNil)

	rsp, ok := f(warningsCmd, req, nil).(*resp)
	c.Assert(ok, check.Equals, true)

	c.Check(rsp.Type, check.Equals, ResponseTypeSync)
	c.Check(rsp.Status, check.Equals, 200)
	c.Assert(rsp.Result, check.NotNil)
	return calls, rsp.Result
}

func (s *apiSuite) TestAllWarnings(c *check.C) {
	calls, result := s.testWarnings(c, true, nil)
	c.Check(calls, check.Equals, "all")
	c.Check(result, check.DeepEquals, []state.Warning{})
}

func (s *apiSuite) TestSomeWarnings(c *check.C) {
	calls, result := s.testWarnings(c, false, nil)
	c.Check(calls, check.Equals, "show")
	c.Check(result, check.DeepEquals, []state.Warning{})
}

func (s *apiSuite) TestAckWarnings(c *check.C) {
	calls, result := s.testWarnings(c, false, bytes.NewReader([]byte(`{"action": "okay", "timestamp": "2006-01-02T15:04:05Z"}`)))
	c.Check(calls, check.Equals, "ok")
	c.Check(result, check.DeepEquals, 0)
}

func (s *apiSuite) TestErrToResponseForChangeConflict(c *check.C) {
	si := &snapInstruction{Action: "frobble", Snaps: []string{"foo"}}

	err := &snapstate.ChangeConflictError{Snap: "foo", ChangeKind: "install"}
	rsp := si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 409,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: `snap "foo" has "install" change in progress`,
			Kind:    errorKindSnapChangeConflict,
			Value: map[string]interface{}{
				"snap-name":   "foo",
				"change-kind": "install",
			},
		},
	})

	// only snap
	err = &snapstate.ChangeConflictError{Snap: "foo"}
	rsp = si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 409,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: `snap "foo" has changes in progress`,
			Kind:    errorKindSnapChangeConflict,
			Value: map[string]interface{}{
				"snap-name": "foo",
			},
		},
	})

	// only kind
	err = &snapstate.ChangeConflictError{Message: "specific error msg", ChangeKind: "some-global-op"}
	rsp = si.errToResponse(err).(*resp)
	c.Check(rsp, check.DeepEquals, &resp{
		Status: 409,
		Type:   ResponseTypeError,
		Result: &errorResult{
			Message: "specific error msg",
			Kind:    errorKindSnapChangeConflict,
			Value: map[string]interface{}{
				"change-kind": "some-global-op",
			},
		},
	})
}

func (s *apiSuite) TestErrToResponse(c *check.C) {
	aie := &snap.AlreadyInstalledError{Snap: "foo"}
	nie := &snap.NotInstalledError{Snap: "foo"}
	cce := &snapstate.ChangeConflictError{Snap: "foo"}
	ndme := &snapstate.SnapNeedsDevModeError{Snap: "foo"}
	nc := &snapstate.SnapNotClassicError{Snap: "foo"}
	nce := &snapstate.SnapNeedsClassicError{Snap: "foo"}
	ncse := &snapstate.SnapNeedsClassicSystemError{Snap: "foo"}
	netoe := fakeNetError{message: "other"}
	nettoute := fakeNetError{message: "timeout", timeout: true}
	nettmpe := fakeNetError{message: "temp", temporary: true}

	e := errors.New("other error")

	makeErrorRsp := func(kind errorKind, err error, value interface{}) Response {
		return SyncResponse(&resp{
			Type:   ResponseTypeError,
			Result: &errorResult{Message: err.Error(), Kind: kind, Value: value},
			Status: 400,
		}, nil)
	}

	tests := []struct {
		err         error
		expectedRsp Response
	}{
		{store.ErrSnapNotFound, SnapNotFound("foo", store.ErrSnapNotFound)},
		{store.ErrNoUpdateAvailable, makeErrorRsp(errorKindSnapNoUpdateAvailable, store.ErrNoUpdateAvailable, "")},
		{store.ErrLocalSnap, makeErrorRsp(errorKindSnapLocal, store.ErrLocalSnap, "")},
		{aie, makeErrorRsp(errorKindSnapAlreadyInstalled, aie, "foo")},
		{nie, makeErrorRsp(errorKindSnapNotInstalled, nie, "foo")},
		{ndme, makeErrorRsp(errorKindSnapNeedsDevMode, ndme, "foo")},
		{nc, makeErrorRsp(errorKindSnapNotClassic, nc, "foo")},
		{nce, makeErrorRsp(errorKindSnapNeedsClassic, nce, "foo")},
		{ncse, makeErrorRsp(errorKindSnapNeedsClassicSystem, ncse, "foo")},
		{cce, SnapChangeConflict(cce)},
		{nettoute, makeErrorRsp(errorKindNetworkTimeout, nettoute, "")},
		{netoe, BadRequest("ERR: %v", netoe)},
		{nettmpe, BadRequest("ERR: %v", nettmpe)},
		{e, BadRequest("ERR: %v", e)},
	}

	for _, t := range tests {
		com := check.Commentf("%v", t.err)
		rsp := errToResponse(t.err, []string{"foo"}, BadRequest, "%s: %v", "ERR")
		c.Check(rsp, check.DeepEquals, t.expectedRsp, com)
	}
}
