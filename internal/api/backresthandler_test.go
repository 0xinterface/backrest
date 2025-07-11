package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/garethgeorge/backrest/gen/go/types"
	v1 "github.com/garethgeorge/backrest/gen/go/v1"
	syncapi "github.com/garethgeorge/backrest/internal/api/syncapi"
	"github.com/garethgeorge/backrest/internal/config"
	"github.com/garethgeorge/backrest/internal/cryptoutil"
	"github.com/garethgeorge/backrest/internal/logstore"
	"github.com/garethgeorge/backrest/internal/oplog"
	"github.com/garethgeorge/backrest/internal/oplog/sqlitestore"
	"github.com/garethgeorge/backrest/internal/orchestrator"
	"github.com/garethgeorge/backrest/internal/resticinstaller"
	"github.com/garethgeorge/backrest/internal/testutil"
	"github.com/hashicorp/go-multierror"
	"golang.org/x/sync/errgroup"
	"google.golang.org/protobuf/proto"
)

func createConfigManager(cfg *v1.Config) *config.ConfigManager {
	return &config.ConfigManager{
		Store: &config.MemoryStore{
			Config: cfg,
		},
	}
}

func TestUpdateConfig(t *testing.T) {
	t.Parallel()

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno: 1234,
	}))

	tests := []struct {
		name    string
		req     *v1.Config
		wantErr bool
		res     *v1.Config
	}{
		{
			name: "bad modno",
			req: &v1.Config{
				Modno: 4321,
			},
			wantErr: true,
		},
		{
			name: "good modno",
			req: &v1.Config{
				Modno:    1234,
				Instance: "test",
			},
			wantErr: false,
			res: &v1.Config{
				Modno:    1235,
				Instance: "test",
			},
		},
		{
			name: "reject when validation fails",
			req: &v1.Config{
				Modno:    1235,
				Instance: "test",
				Repos: []*v1.Repo{
					{},
				},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			res, err := sut.handler.SetConfig(ctx, connect.NewRequest(tt.req))
			if (err != nil) != tt.wantErr {
				t.Errorf("SetConfig() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if err == nil && !proto.Equal(res.Msg, tt.res) {
				t.Errorf("SetConfig() got = %v, want %v", res, tt.res)
			}
		})
	}
}

func TestRemoveRepo(t *testing.T) {
	t.Parallel()

	mgr := createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
			},
		},
	})
	sut := createSystemUnderTest(t, mgr)

	// insert an operation that should get removed
	if _, err := sut.handler.RunCommand(context.Background(), connect.NewRequest(&v1.RunCommandRequest{
		RepoId:  "local",
		Command: "help",
	})); err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}

	// assert that the operation exists
	ops := getOperations(t, sut.oplog)
	if len(ops) != 1 {
		t.Fatalf("expected 1 operation, got %d", len(ops))
	}

	if _, err := sut.handler.RemoveRepo(context.Background(), connect.NewRequest(&types.StringValue{
		Value: "local",
	})); err != nil {
		t.Fatalf("RemoveRepo() error = %v", err)
	}

	// assert that the operation has been removed
	ops = getOperations(t, sut.oplog)
	if len(ops) != 0 {
		t.Fatalf("expected 0 operations, got %d", len(ops))
	}
}

func TestBackup(t *testing.T) {
	t.Parallel()

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Retention: &v1.RetentionPolicy{
					Policy: &v1.RetentionPolicy_PolicyKeepLastN{PolicyKeepLastN: 100},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	_, err := sut.handler.Backup(ctx, connect.NewRequest(&types.StringValue{Value: "test"}))
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}

	// Wait for the backup to complete.
	if err := testutil.Retry(t, ctx, func() error {
		ops := getOperations(t, sut.oplog)
		if slices.IndexFunc(ops, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationBackup)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}) == -1 {
			return fmt.Errorf("expected a backup operation, got %v", ops)
		}
		return nil
	}); err != nil {
		t.Fatalf("Couldn't find backup operation in oplog")
	}

	// Wait for the index snapshot operation to appear in the oplog.
	var snapshotOp *v1.Operation
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationIndexSnapshot)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			snapshotOp = operations[index]
			return nil
		}
		return errors.New("snapshot not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find snapshot in oplog")
	}

	if snapshotOp.SnapshotId == "" {
		t.Fatalf("snapshotId must be set")
	}

	// Wait for a forget operation to appear in the oplog.
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationForget)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			op := operations[index]
			if op.FlowId != snapshotOp.FlowId {
				t.Fatalf("Flow ID mismatch on forget operation")
			}
			return nil
		}
		return errors.New("forget not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find forget in oplog")
	}
}

func TestMultipleBackup(t *testing.T) {
	t.Parallel()

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Retention: &v1.RetentionPolicy{
					Policy: &v1.RetentionPolicy_PolicyKeepLastN{
						PolicyKeepLastN: 1,
					},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()

	go func() {
		sut.orch.Run(ctx)
	}()

	for i := 0; i < 2; i++ {
		_, err := sut.handler.Backup(ctx, connect.NewRequest(&types.StringValue{Value: "test"}))
		if err != nil {
			t.Fatalf("Backup() error = %v", err)
		}
	}

	// Wait for a forget that removed 1 snapshot to appear in the oplog
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			forget, ok := op.GetOp().(*v1.Operation_OperationForget)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok && len(forget.OperationForget.Forget) > 0
		}); index == -1 {
			return errors.New("forget not indexed")
		} else if len(operations[index].GetOp().(*v1.Operation_OperationForget).OperationForget.Forget) != 1 {
			return fmt.Errorf("expected 1 item removed in the forget operation, got %d", len(operations[index].GetOp().(*v1.Operation_OperationForget).OperationForget.Forget))
		}
		return nil
	}); err != nil {
		t.Fatalf("Couldn't find forget with 1 item removed in the operation log")
	}
}

func TestHookExecution(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	hookOutputBefore := path.Join(dir, "before.txt")
	hookOutputAfter := path.Join(dir, "after.txt")

	commandBefore := fmt.Sprintf("echo before > %s", hookOutputBefore)
	commandAfter := fmt.Sprintf("echo after > %s", hookOutputAfter)
	if runtime.GOOS == "windows" {
		commandBefore = fmt.Sprintf("echo \"before\" | Out-File -FilePath %q", hookOutputBefore)
		commandAfter = fmt.Sprintf("echo \"after\" | Out-File -FilePath %q", hookOutputAfter)
	}

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: commandBefore,
							},
						},
					},
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_END,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: commandAfter,
							},
						},
					},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	_, err := sut.handler.Backup(ctx, connect.NewRequest(&types.StringValue{Value: "test"}))
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}

	// Wait for two hook operations to appear in the oplog
	if err := testutil.Retry(t, ctx, func() error {
		hookOps := slices.DeleteFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationRunHook)
			return !ok
		})

		if len(hookOps) == 2 {
			return nil
		}
		return fmt.Errorf("expected 2 hook operations, got %d", len(hookOps))
	}); err != nil {
		t.Fatalf("Couldn't find hooks in oplog: %v", err)
	}

	// expect the hook output files to exist
	if _, err := os.Stat(hookOutputBefore); err != nil {
		t.Fatalf("hook output file before not found")
	}

	if _, err := os.Stat(hookOutputAfter); err != nil {
		t.Fatalf("hook output file after not found")
	}
}

func TestHookOnErrorHandling(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("skipping test on windows")
	}

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test-cancel",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: "exit 123",
							},
						},
						OnError: v1.Hook_ON_ERROR_CANCEL,
					},
				},
			},
			{
				Id:   "test-error",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: "exit 123",
							},
						},
						OnError: v1.Hook_ON_ERROR_FATAL,
					},
				},
			},
			{
				Id:   "test-ignore",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: "exit 123",
							},
						},
						OnError: v1.Hook_ON_ERROR_IGNORE,
					},
				},
			},
			{
				Id:   "test-retry",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: "exit 123",
							},
						},
						OnError: v1.Hook_ON_ERROR_RETRY_10MINUTES,
					},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	tests := []struct {
		name             string
		plan             string
		wantHookStatus   v1.OperationStatus
		wantBackupStatus v1.OperationStatus
		wantBackupError  bool
		noWaitForBackup  bool
	}{
		{
			name:             "cancel",
			plan:             "test-cancel",
			wantHookStatus:   v1.OperationStatus_STATUS_ERROR,
			wantBackupStatus: v1.OperationStatus_STATUS_USER_CANCELLED,
			wantBackupError:  true,
		},
		{
			name:             "error",
			plan:             "test-error",
			wantHookStatus:   v1.OperationStatus_STATUS_ERROR,
			wantBackupStatus: v1.OperationStatus_STATUS_ERROR,
			wantBackupError:  true,
		},
		{
			name:             "ignore",
			plan:             "test-ignore",
			wantHookStatus:   v1.OperationStatus_STATUS_ERROR,
			wantBackupStatus: v1.OperationStatus_STATUS_SUCCESS,
			wantBackupError:  false,
		},
		{
			name:             "retry",
			plan:             "test-retry",
			wantHookStatus:   v1.OperationStatus_STATUS_ERROR,
			wantBackupStatus: v1.OperationStatus_STATUS_PENDING,
			wantBackupError:  false,
			noWaitForBackup:  true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			sut.opstore.ResetForTest(t)

			var errgroup errgroup.Group

			errgroup.Go(func() error {
				_, err := sut.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: tc.plan}))
				if (err != nil) != tc.wantBackupError {
					return fmt.Errorf("Backup() error = %v, wantErr %v", err, tc.wantBackupError)
				}
				return nil
			})

			if !tc.noWaitForBackup {
				if err := errgroup.Wait(); err != nil {
					t.Fatalf("%s", err.Error())
				}
			}

			// Wait for hook operation to be attempted in the oplog
			if err := testutil.Retry(t, ctx, func() error {
				hookOps := slices.DeleteFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
					_, ok := op.GetOp().(*v1.Operation_OperationRunHook)
					return !ok
				})
				if len(hookOps) != 1 {
					return fmt.Errorf("expected 1 hook operations, got %d", len(hookOps))
				}
				if hookOps[0].Status != tc.wantHookStatus {
					return fmt.Errorf("expected hook operation error status, got %v", hookOps[0].Status)
				}
				return nil
			}); err != nil {
				t.Fatalf("Couldn't find hook operation in oplog: %v", err)
			}

			if err := testutil.Retry(t, ctx, func() error {
				backupOps := slices.DeleteFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
					_, ok := op.GetOp().(*v1.Operation_OperationBackup)
					return !ok
				})
				if len(backupOps) != 1 {
					return fmt.Errorf("expected 1 backup operation, got %d", len(backupOps))
				}
				if backupOps[0].Status != tc.wantBackupStatus {
					return fmt.Errorf("expected backup operation status %v, got %v", tc.wantBackupStatus, backupOps[0].Status)
				}
				return nil
			}); err != nil {
				t.Fatalf("Failed to verify backup operation: %v", err)
			}
		})
	}
}

func TestCancelBackup(t *testing.T) {
	t.Parallel()

	// a hook is used to make the backup operation wait long enough to be cancelled
	hookCmd := "sleep 2"
	if runtime.GOOS == "windows" {
		hookCmd = "Start-Sleep -Seconds 2"
	}

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test",
				Repo: "local",
				Paths: []string{
					t.TempDir(),
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Retention: &v1.RetentionPolicy{
					Policy: &v1.RetentionPolicy_PolicyKeepLastN{
						PolicyKeepLastN: 1,
					},
				},
				Hooks: []*v1.Hook{
					{
						Conditions: []v1.Hook_Condition{
							v1.Hook_CONDITION_SNAPSHOT_START,
						},
						Action: &v1.Hook_ActionCommand{
							ActionCommand: &v1.Hook_Command{
								Command: hookCmd,
							},
						},
					},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	go func() {
		backupReq := &types.StringValue{Value: "test"}
		_, err := sut.handler.Backup(ctx, connect.NewRequest(backupReq))
		if err != nil {
			t.Logf("Backup() error = %v", err)
		}
	}()

	// Find the in-progress backup operation ID in the oplog, waits for the task to be in progress before attempting to cancel.
	var backupOpId int64
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		for _, op := range operations {
			if op.GetOperationBackup() != nil && op.Status == v1.OperationStatus_STATUS_INPROGRESS {
				backupOpId = op.Id
				return nil
			}
		}
		return errors.New("backup operation not found")
	}); err != nil {
		t.Fatalf("Couldn't find backup operation in oplog")
	}

	if _, err := sut.handler.Cancel(context.Background(), connect.NewRequest(&types.Int64Value{Value: backupOpId})); err != nil {
		t.Errorf("Cancel() error = %v, wantErr nil", err)
	}

	if err := testutil.Retry(t, ctx, func() error {
		if slices.IndexFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationBackup)
			return op.Status == v1.OperationStatus_STATUS_ERROR && ok
		}) == -1 {
			return errors.New("backup operation not found")
		}
		return nil
	}); err != nil {
		t.Fatalf("Couldn't find failed canceled backup operation in oplog")
	}
}

func TestRestore(t *testing.T) {
	t.Parallel()

	backupDataDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(backupDataDir, "findme.txt"), []byte("test data"), 0644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
		Plans: []*v1.Plan{
			{
				Id:   "test",
				Repo: "local",
				Paths: []string{
					backupDataDir,
				},
				Schedule: &v1.Schedule{
					Schedule: &v1.Schedule_Disabled{Disabled: true},
				},
				Retention: &v1.RetentionPolicy{
					Policy: &v1.RetentionPolicy_PolicyKeepAll{PolicyKeepAll: true},
				},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	_, err := sut.handler.Backup(ctx, connect.NewRequest(&types.StringValue{Value: "test"}))
	if err != nil {
		t.Fatalf("Backup() error = %v", err)
	}

	// Wait for the backup to complete.
	if err := testutil.Retry(t, ctx, func() error {
		// Check that there is a successful backup recorded in the log.
		if slices.IndexFunc(getOperations(t, sut.oplog), func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationBackup)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}) == -1 {
			return errors.New("Expected a backup operation")
		}
		return nil
	}); err != nil {
		t.Fatalf("Couldn't find backup operation in oplog")
	}

	// Wait for the index snapshot operation to appear in the oplog.
	var snapshotOp *v1.Operation
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationIndexSnapshot)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			snapshotOp = operations[index]
			return nil
		}
		return errors.New("snapshot not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find snapshot in oplog")
	}

	if snapshotOp.SnapshotId == "" {
		t.Fatalf("snapshotId must be set")
	}

	restoreTarget := t.TempDir() + "/restore"

	_, err = sut.handler.Restore(ctx, connect.NewRequest(&v1.RestoreSnapshotRequest{
		SnapshotId: snapshotOp.SnapshotId,
		PlanId:     "test",
		RepoId:     "local",
		Target:     restoreTarget,
	}))
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}

	// Wait for a restore operation to appear in the oplog.
	if err := testutil.Retry(t, ctx, func() error {
		operations := getOperations(t, sut.oplog)
		if index := slices.IndexFunc(operations, func(op *v1.Operation) bool {
			_, ok := op.GetOp().(*v1.Operation_OperationRestore)
			return op.Status == v1.OperationStatus_STATUS_SUCCESS && ok
		}); index != -1 {
			op := operations[index]
			if op.SnapshotId != snapshotOp.SnapshotId {
				t.Errorf("Snapshot ID mismatch on restore operation")
			}
			return nil
		}
		return errors.New("restore not indexed")
	}); err != nil {
		t.Fatalf("Couldn't find restore in oplog")
	}

	// Check that the restore target contains the expected file.
	var files []string
	filepath.Walk(restoreTarget, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			t.Errorf("Walk() error = %v", err)
		}
		files = append(files, path)
		return nil
	})
	t.Logf("files: %v", files)
	if !slices.ContainsFunc(files, func(s string) bool {
		return strings.HasSuffix(s, "findme.txt")
	}) {
		t.Fatalf("Expected file not found in restore target")
	}
}

func TestRunCommand(t *testing.T) {
	testutil.InstallZapLogger(t)
	sut := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test",
		Repos: []*v1.Repo{
			{
				Id:       "local",
				Guid:     cryptoutil.MustRandomID(cryptoutil.DefaultIDBits),
				Uri:      t.TempDir(),
				Password: "test",
				Flags:    []string{"--no-cache"},
			},
		},
	}))

	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()
	go func() {
		sut.orch.Run(ctx)
	}()

	res, err := sut.handler.RunCommand(ctx, connect.NewRequest(&v1.RunCommandRequest{
		RepoId:  "local",
		Command: "help",
	}))
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	op, err := sut.oplog.Get(res.Msg.Value)
	if err != nil {
		t.Fatalf("Failed to find runcommand operation: %v", err)
	}

	if op.Status != v1.OperationStatus_STATUS_SUCCESS {
		t.Fatalf("Expected runcommand operation to succeed")
	}

	cmdOp := op.GetOperationRunCommand()
	if cmdOp == nil {
		t.Fatalf("Expected runcommand operation to be of type OperationRunCommand")
	}
	if cmdOp.Command != "help" {
		t.Fatalf("Expected runcommand operation to have correct command")
	}
	if cmdOp.OutputLogref == "" {
		t.Fatalf("Expected runcommand operation to have output logref")
	}

	log, err := sut.logStore.Open(cmdOp.OutputLogref)
	if err != nil {
		t.Fatalf("Failed to open log: %v", err)
	}
	defer log.Close()

	data, err := io.ReadAll(log)
	if err != nil {
		t.Fatalf("Failed to read log: %v", err)
	}
	if !strings.Contains(string(data), "Usage") {
		t.Fatalf("Expected log output to contain help text")
	}
}

func TestMultihostIndexSnapshots(t *testing.T) {
	t.Parallel()
	ctx, cancel := testutil.WithDeadlineFromTest(t, context.Background())
	defer cancel()

	emptyDir := t.TempDir()
	repoGUID := cryptoutil.MustRandomID(cryptoutil.DefaultIDBits)

	repo1 := &v1.Repo{
		Id:       "local1",
		Guid:     repoGUID,
		Uri:      t.TempDir(),
		Password: "test",
		Flags:    []string{"--no-cache"},
	}
	repo2 := proto.Clone(repo1).(*v1.Repo)
	repo2.Id = "local2"

	plan1 := &v1.Plan{
		Id:   "test1",
		Repo: "local1",
		Paths: []string{
			emptyDir,
		},
		Schedule: &v1.Schedule{
			Schedule: &v1.Schedule_Disabled{Disabled: true},
		},
	}
	plan2 := proto.Clone(plan1).(*v1.Plan)
	plan2.Id = "test2"
	plan2.Repo = "local2"

	host1 := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test1",
		Repos: []*v1.Repo{
			repo1,
		},
		Plans: []*v1.Plan{
			plan1,
		},
	}))
	go func() {
		host1.orch.Run(ctx)
	}()

	host2 := createSystemUnderTest(t, createConfigManager(&v1.Config{
		Modno:    1234,
		Instance: "test2",
		Repos: []*v1.Repo{
			repo2,
		},
		Plans: []*v1.Plan{
			plan2,
		},
	}))
	go func() {
		host2.orch.Run(ctx)
	}()

	host1.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: "test1"}))
	host2.handler.Backup(context.Background(), connect.NewRequest(&types.StringValue{Value: "test2"}))

	for i := 0; i < 1; i++ {
		if _, err := host1.handler.IndexSnapshots(context.Background(), connect.NewRequest(&types.StringValue{Value: "local1"})); err != nil {
			t.Errorf("local1 sut1 IndexSnapshots() error = %v", err)
		}
		if _, err := host2.handler.IndexSnapshots(context.Background(), connect.NewRequest(&types.StringValue{Value: "local2"})); err != nil {
			t.Errorf("local2 sut2 IndexSnapshots() error = %v", err)
		}
	}

	findSnapshotsFromInstance := func(ops []*v1.Operation, inst string) []*v1.Operation {
		output := []*v1.Operation{}
		for _, op := range ops {
			if op.GetOperationIndexSnapshot() != nil && op.InstanceId == inst {
				output = append(output, op)
			}
		}
		return output
	}

	countSnapshotOperations := func(ops []*v1.Operation) int {
		count := 0
		for _, op := range ops {
			if op.GetOperationIndexSnapshot() != nil {
				count++
			}
		}
		return count
	}

	var ops []*v1.Operation
	var ops2 []*v1.Operation
	testutil.TryNonfatal(t, ctx, func() error {
		ops = getOperations(t, host1.oplog)
		ops2 = getOperations(t, host2.oplog)
		var err error
		for _, logOps := range []struct {
			ops      []*v1.Operation
			instance string
		}{
			{ops, "test1"},
			{ops, "test2"},
			{ops2, "test1"},
			{ops2, "test2"},
		} {
			snapshotOps := findSnapshotsFromInstance(logOps.ops, logOps.instance)
			if len(snapshotOps) != 1 {
				err = multierror.Append(err, fmt.Errorf("expected exactly 1 snapshot from %s, got %d", logOps.instance, len(snapshotOps)))
			}
		}
		return err
	})

	if countSnapshotOperations(ops) != 2 {
		t.Errorf("expected exactly 2 snapshot operation in sut1 log, got %d", countSnapshotOperations(ops))
	}
	if countSnapshotOperations(ops2) != 2 {
		t.Errorf("expected exactly 2 snapshot operation in sut2 log, got %d", countSnapshotOperations(ops2))
	}
}

type systemUnderTest struct {
	handler  *BackrestHandler
	oplog    *oplog.OpLog
	opstore  *sqlitestore.SqliteStore
	orch     *orchestrator.Orchestrator
	logStore *logstore.LogStore
	config   *v1.Config
}

func createSystemUnderTest(t *testing.T, config *config.ConfigManager) systemUnderTest {
	dir := t.TempDir()

	cfg, err := config.Get()
	if err != nil {
		t.Fatalf("Failed to get config: %v", err)
	}
	peerStateManager := syncapi.NewInMemoryPeerStateManager()
	resticBin, err := resticinstaller.FindOrInstallResticBinary()
	if err != nil {
		t.Fatalf("Failed to find or install restic binary: %v", err)
	}
	opstore, err := sqlitestore.NewSqliteStore(filepath.Join(dir, "oplog.sqlite"))
	if err != nil {
		t.Fatalf("Failed to create opstore: %v", err)
	}
	t.Cleanup(func() { opstore.Close() })
	oplog, err := oplog.NewOpLog(opstore)
	if err != nil {
		t.Fatalf("Failed to create oplog: %v", err)
	}
	logStore, err := logstore.NewLogStore(filepath.Join(dir, "tasklogs"))
	if err != nil {
		t.Fatalf("Failed to create log store: %v", err)
	}
	t.Cleanup(func() { logStore.Close() })
	orch, err := orchestrator.NewOrchestrator(
		resticBin, config, oplog, logStore,
	)
	if err != nil {
		t.Fatalf("Failed to create orchestrator: %v", err)
	}

	for _, repo := range cfg.Repos {
		rorch, err := orch.GetRepoOrchestrator(repo.Id)
		if err != nil {
			t.Fatalf("Failed to get repo %s: %v", repo.Id, err)
		}

		if err := rorch.Init(context.Background()); err != nil {
			t.Fatalf("Failed to init repo %s: %v", repo.Id, err)
		}
	}

	h := NewBackrestHandler(config, peerStateManager, orch, oplog, logStore)

	return systemUnderTest{
		handler:  h,
		oplog:    oplog,
		opstore:  opstore,
		orch:     orch,
		logStore: logStore,
		config:   cfg,
	}
}

func getOperations(t *testing.T, log *oplog.OpLog) []*v1.Operation {
	operations := []*v1.Operation{}
	if err := log.Query(oplog.SelectAll, func(op *v1.Operation) error {
		operations = append(operations, op)
		return nil
	}); err != nil {
		t.Fatalf("Failed to read oplog: %v", err)
	}
	return operations
}
