/*
Copyright 2019-2020 vChain, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package auditor

import (
	"context"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/codenotary/immudb/pkg/api/schema"
	"github.com/codenotary/immudb/pkg/auth"
	"github.com/codenotary/immudb/pkg/client"
	"github.com/codenotary/immudb/pkg/client/cache"
	"github.com/codenotary/immudb/pkg/client/timestamp"
	"github.com/codenotary/immudb/pkg/logger"
	"github.com/golang/protobuf/ptypes/empty"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

type Auditor interface {
	Run(interval time.Duration, stopc <-chan struct{}, donec chan<- struct{})
}

type defaultAuditor struct {
	index         uint64
	logger        logger.Logger
	serverAddress string
	dialOptions   []grpc.DialOption
	history       cache.HistoryCache
	ts            client.TimestampService
	username      []byte
	password      []byte
	slugifyRegExp *regexp.Regexp
	updateMetrics func(string, string, bool, *schema.Root, *schema.Root)
}

func DefaultAuditor(
	interval time.Duration,
	serverAddress string,
	dialOptions *[]grpc.DialOption,
	username string,
	password string,
	history cache.HistoryCache,
	updateMetrics func(string, string, bool, *schema.Root, *schema.Root),
) (Auditor, error) {
	logr := logger.NewSimpleLogger("auditor", os.Stderr)
	dt, err := timestamp.NewTdefault()
	if err != nil {
		return nil, err
	}
	slugifyRegExp, err := regexp.Compile(`[^a-zA-Z0-9\-_]+`)
	if err != nil {
		logr.Warningf("error compiling regex for slugifier: %v", err)
	}
	return &defaultAuditor{
		0,
		logr,
		serverAddress,
		*dialOptions,
		history,
		client.NewTimestampService(dt),
		[]byte(username),
		[]byte(password),
		slugifyRegExp,
		updateMetrics,
	}, nil
}

func (a *defaultAuditor) Run(
	interval time.Duration,
	stopc <-chan struct{},
	donec chan<- struct{},
) {
	defer func() { donec <- struct{}{} }()
	a.logger.Infof("starting auditor with a %s interval ...", interval)
	err := repeat(interval, stopc, a.audit)
	if err != nil {
		return
	}
	a.logger.Infof("auditor stopped")
}

func (a *defaultAuditor) audit() error {
	start := time.Now()
	a.index++
	a.logger.Infof("audit #%d started @ %s", a.index, start)

	// returning an error would completely stop the auditor process
	var noErr error

	ctx := context.Background()
	conn, err := a.connect(ctx)
	if err != nil {
		return noErr
	}
	defer a.closeConnection(conn)
	serviceClient := schema.NewImmuServiceClient(conn)

	root, err := serviceClient.CurrentRoot(ctx, &empty.Empty{})
	if err != nil {
		a.logger.Errorf("error getting current root: %v", err)
		return noErr
	}

	isEmptyDB := len(root.GetRoot()) == 0 && root.GetIndex() == 0

	verified := true
	serverID := a.getServerID(ctx, serviceClient)
	prevRoot, err := a.history.Get(serverID)
	if err != nil {
		a.logger.Errorf(err.Error())
		return noErr
	}
	if prevRoot != nil {
		if isEmptyDB {
			a.logger.Errorf(
				"audit #%d aborted: database is empty on server %s @ %s, "+
					"but locally a previous root exists with hash %x at index %d",
				a.index, serverID, a.serverAddress, prevRoot.Root, prevRoot.Index)
			return noErr
		}
		proof, err := serviceClient.Consistency(ctx, &schema.Index{
			Index: prevRoot.GetIndex(),
		})
		if err != nil {
			a.logger.Errorf(
				"error fetching consistency proof for previous root %d: %v",
				prevRoot.GetIndex(), err)
			return noErr
		}
		verified =
			proof.Verify(schema.Root{Index: prevRoot.Index, Root: prevRoot.Root})
		firstRoot := proof.FirstRoot
		// TODO OGG: clarify with team: why proof.FirstRoot is empty if check fails
		if !verified && len(firstRoot) == 0 {
			firstRoot = prevRoot.GetRoot()
		}
		a.logger.Infof("audit #%d result:\n  consistent:	%t\n"+
			"  firstRoot:	%x at index: %d\n  secondRoot:	%x at index: %d",
			a.index, verified, firstRoot, proof.First, proof.SecondRoot, proof.Second)
		root = &schema.Root{Index: proof.Second, Root: proof.SecondRoot}
		a.updateMetrics(serverID, a.serverAddress, verified, prevRoot, root)
	} else if isEmptyDB {
		a.logger.Warningf("audit #%d canceled: database is empty on server %s @ %s",
			a.index, serverID, a.serverAddress)
		return noErr
	}

	if !verified {
		a.logger.Warningf(
			"audit #%d detected possible tampering of remote root (at index %d) "+
				"so it will not overwrite the previous local root (at index %d)",
			a.index, root.GetIndex(), prevRoot.GetIndex())
	} else if prevRoot == nil || root.GetIndex() != prevRoot.GetIndex() {
		if err := a.history.Set(root, serverID); err != nil {
			a.logger.Errorf(err.Error())
			return noErr
		}
	}
	a.logger.Infof("audit #%d finished in %s @ %s",
		a.index, time.Since(start), time.Now().Format(time.RFC3339Nano))
	return noErr
}

func (a *defaultAuditor) connect(ctx context.Context) (*grpc.ClientConn, error) {
	conn, err := grpc.Dial(a.serverAddress, a.dialOptions...)
	if err != nil {
		a.logger.Errorf(
			"error dialing (pre-login) to immudb @ %s: %v", a.serverAddress, err)
		return nil, err
	}
	defer a.closeConnection(conn)
	serviceClient := schema.NewImmuServiceClient(conn)
	loginResponse, err := serviceClient.Login(ctx, &schema.LoginRequest{
		User:     a.username,
		Password: a.password,
	})
	if err != nil {
		grpcStatus, ok1 := status.FromError(err)
		authDisabled, ok2 := status.FromError(auth.ErrServerAuthDisabled)
		if !ok1 || !ok2 || grpcStatus.Code() != authDisabled.Code() ||
			grpcStatus.Message() != authDisabled.Message() {
			a.logger.Errorf("error logging in: %v", err)
			return nil, err
		}
	}
	if loginResponse != nil {
		token := string(loginResponse.GetToken())
		a.dialOptions = append(a.dialOptions,
			grpc.WithUnaryInterceptor(auth.ClientUnaryInterceptor(token)))
	}
	connWithToken, err := grpc.Dial(a.serverAddress, a.dialOptions...)
	if err != nil {
		a.logger.Errorf(
			"error dialing to immudb @ %s: %v", a.serverAddress, err)
		return nil, err
	}
	return connWithToken, nil
}

func (a *defaultAuditor) getServerID(
	ctx context.Context,
	serviceClient schema.ImmuServiceClient,
) string {
	serverID, err := client.GetServerUuid(ctx, serviceClient)
	if err != nil {
		if err != client.ErrNoServerUuid {
			a.logger.Errorf("error getting server UUID: %v", err)
		} else {
			a.logger.Warningf(err.Error())
		}
	}
	if serverID == "" {
		serverID = strings.ReplaceAll(
			strings.ReplaceAll(a.serverAddress, ".", "-"),
			":", "_")
		serverID = a.slugifyRegExp.ReplaceAllString(serverID, "")
		a.logger.Debugf(
			"the current immudb server @ %s will be identified as %s",
			a.serverAddress, serverID)
	}
	return serverID
}

func (a *defaultAuditor) closeConnection(conn *grpc.ClientConn) {
	if err := conn.Close(); err != nil {
		a.logger.Errorf("error closing connection: %v", err)
	}
}

// repeat executes f every interval until stopc is closed or f returns an error.
// It executes f once right after being called.
func repeat(
	interval time.Duration,
	stopc <-chan struct{},
	f func() error,
) error {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		if err := f(); err != nil {
			return err
		}
		select {
		case <-stopc:
			return nil
		case <-tick.C:
		}
	}
}
