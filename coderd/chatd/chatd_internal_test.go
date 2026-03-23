package chatd

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"golang.org/x/xerrors"

	"cdr.dev/slog/v3/sloggers/slogtest"
	"github.com/coder/coder/v2/coderd/database"
	"github.com/coder/coder/v2/coderd/database/dbmock"
	dbpubsub "github.com/coder/coder/v2/coderd/database/pubsub"
	coderdpubsub "github.com/coder/coder/v2/coderd/pubsub"
	"github.com/coder/coder/v2/codersdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk"
	"github.com/coder/coder/v2/codersdk/workspacesdk/agentconnmock"
)

func TestRefreshChatWorkspaceSnapshot_NoReloadWhenWorkspacePresent(t *testing.T) {
	t.Parallel()

	workspaceID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
	}

	calls := 0
	refreshed, err := refreshChatWorkspaceSnapshot(
		context.Background(),
		chat,
		func(context.Context, uuid.UUID) (database.Chat, error) {
			calls++
			return database.Chat{}, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, chat, refreshed)
	require.Equal(t, 0, calls)
}

func TestRefreshChatWorkspaceSnapshot_ReloadsWhenWorkspaceMissing(t *testing.T) {
	t.Parallel()

	chatID := uuid.New()
	workspaceID := uuid.New()
	chat := database.Chat{ID: chatID}
	reloaded := database.Chat{
		ID: chatID,
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
	}

	calls := 0
	refreshed, err := refreshChatWorkspaceSnapshot(
		context.Background(),
		chat,
		func(_ context.Context, id uuid.UUID) (database.Chat, error) {
			calls++
			require.Equal(t, chatID, id)
			return reloaded, nil
		},
	)
	require.NoError(t, err)
	require.Equal(t, reloaded, refreshed)
	require.Equal(t, 1, calls)
}

func TestRefreshChatWorkspaceSnapshot_ReturnsReloadError(t *testing.T) {
	t.Parallel()

	chat := database.Chat{ID: uuid.New()}
	loadErr := xerrors.New("boom")

	refreshed, err := refreshChatWorkspaceSnapshot(
		context.Background(),
		chat,
		func(context.Context, uuid.UUID) (database.Chat, error) {
			return database.Chat{}, loadErr
		},
	)
	require.Error(t, err)
	require.ErrorContains(t, err, "reload chat workspace state")
	require.ErrorContains(t, err, loadErr.Error())
	require.Equal(t, chat, refreshed)
}

func TestResolveInstructionsReusesTurnLocalWorkspaceAgent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceID := uuid.New()
	agentID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
		AgentID: uuid.NullUUID{
			UUID:  agentID,
			Valid: true,
		},
	}
	workspaceAgent := database.WorkspaceAgent{
		ID:                agentID,
		OperatingSystem:   "linux",
		Directory:         "/home/coder/project",
		ExpandedDirectory: "/home/coder/project",
	}

	db.EXPECT().GetWorkspaceAgentByID(
		gomock.Any(),
		agentID,
	).Return(workspaceAgent, nil).Times(1)

	conn := agentconnmock.NewMockAgentConn(ctrl)
	conn.EXPECT().SetExtraHeaders(gomock.Any()).Times(1)
	conn.EXPECT().LS(gomock.Any(), "", gomock.Any()).Return(
		workspacesdk.LSResponse{},
		codersdk.NewTestError(404, "POST", "/api/v0/list-directory"),
	).Times(1)
	conn.EXPECT().ReadFile(
		gomock.Any(),
		"/home/coder/project/AGENTS.md",
		int64(0),
		int64(maxInstructionFileBytes+1),
	).Return(
		nil,
		"",
		codersdk.NewTestError(404, "GET", "/api/v0/read-file"),
	).Times(1)

	logger := slogtest.Make(t, &slogtest.Options{IgnoreErrors: true})
	server := &Server{
		db:               db,
		logger:           logger,
		instructionCache: make(map[uuid.UUID]cachedInstruction),
		agentConnFn: func(context.Context, uuid.UUID) (workspacesdk.AgentConn, func(), error) {
			return conn, func() {}, nil
		},
	}

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           server,
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	t.Cleanup(workspaceCtx.close)

	instruction := server.resolveInstructions(
		ctx,
		chat,
		workspaceCtx.getWorkspaceAgent,
		workspaceCtx.getWorkspaceConn,
	)
	require.Contains(t, instruction, "Operating System: linux")
	require.Contains(t, instruction, "Working Directory: /home/coder/project")
}

func TestTurnWorkspaceContext_BindingFirstPath(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceID := uuid.New()
	agentID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
		AgentID: uuid.NullUUID{
			UUID:  agentID,
			Valid: true,
		},
	}
	workspaceAgent := database.WorkspaceAgent{ID: agentID}

	db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), agentID).Return(workspaceAgent, nil).Times(1)

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           &Server{db: db},
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	t.Cleanup(workspaceCtx.close)

	chatSnapshot, agent, err := workspaceCtx.ensureWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, chat, chatSnapshot)
	require.Equal(t, workspaceAgent, agent)

	gotAgent, err := workspaceCtx.getWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, workspaceAgent, gotAgent)
	require.Equal(t, chat, currentChat)
}

func TestTurnWorkspaceContext_NullBindingLazyBind(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceID := uuid.New()
	buildID := uuid.New()
	agentID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
	}
	workspaceAgent := database.WorkspaceAgent{ID: agentID}
	updatedChat := chat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: agentID, Valid: true}

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceID).Return([]database.WorkspaceAgent{workspaceAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceID).Return(database.WorkspaceBuild{ID: buildID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ExpectedWorkspaceID: workspaceID,
			BuildID:             uuid.NullUUID{UUID: buildID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: agentID, Valid: true},
			ID:                  chat.ID,
		}).Return(updatedChat, nil),
	)

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           &Server{db: db},
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	t.Cleanup(workspaceCtx.close)

	chatSnapshot, agent, err := workspaceCtx.ensureWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, updatedChat, chatSnapshot)
	require.Equal(t, workspaceAgent, agent)
	require.Equal(t, updatedChat, currentChat)

	gotAgent, err := workspaceCtx.getWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, workspaceAgent, gotAgent)
}

func TestTurnWorkspaceContext_StaleBindingRepair(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceID := uuid.New()
	staleAgentID := uuid.New()
	buildID := uuid.New()
	currentAgentID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
		AgentID: uuid.NullUUID{
			UUID:  staleAgentID,
			Valid: true,
		},
	}
	currentAgent := database.WorkspaceAgent{ID: currentAgentID}
	updatedChat := chat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: currentAgentID, Valid: true}

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), staleAgentID).Return(database.WorkspaceAgent{}, xerrors.New("missing agent")),
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceID).Return([]database.WorkspaceAgent{currentAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceID).Return(database.WorkspaceBuild{ID: buildID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ExpectedWorkspaceID: workspaceID,
			BuildID:             uuid.NullUUID{UUID: buildID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: currentAgentID, Valid: true},
			ID:                  chat.ID,
		}).Return(updatedChat, nil),
	)

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           &Server{db: db},
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	t.Cleanup(workspaceCtx.close)

	chatSnapshot, agent, err := workspaceCtx.ensureWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, updatedChat, chatSnapshot)
	require.Equal(t, currentAgent, agent)
	require.Equal(t, updatedChat, currentChat)
}

func TestTurnWorkspaceContextGetWorkspaceConnLazyValidationSwitchesWorkspaceAgent(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceID := uuid.New()
	staleAgentID := uuid.New()
	currentAgentID := uuid.New()
	buildID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceID,
			Valid: true,
		},
		AgentID: uuid.NullUUID{
			UUID:  staleAgentID,
			Valid: true,
		},
	}
	staleAgent := database.WorkspaceAgent{ID: staleAgentID}
	currentAgent := database.WorkspaceAgent{ID: currentAgentID}
	updatedChat := chat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: currentAgentID, Valid: true}

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), staleAgentID).Return(staleAgent, nil),
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceID).Return([]database.WorkspaceAgent{currentAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceID).Return(database.WorkspaceBuild{ID: buildID}, nil),
		db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), currentAgentID).Return(currentAgent, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ExpectedWorkspaceID: workspaceID,
			BuildID:             uuid.NullUUID{UUID: buildID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: currentAgentID, Valid: true},
			ID:                  chat.ID,
		}).Return(updatedChat, nil),
	)

	conn := agentconnmock.NewMockAgentConn(ctrl)
	conn.EXPECT().SetExtraHeaders(gomock.Any()).Times(1)

	var dialed []uuid.UUID
	server := &Server{db: db}
	server.agentConnFn = func(_ context.Context, agentID uuid.UUID) (workspacesdk.AgentConn, func(), error) {
		dialed = append(dialed, agentID)
		if agentID == staleAgentID {
			return nil, nil, xerrors.New("dial failed")
		}
		return conn, func() {}, nil
	}

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           server,
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	t.Cleanup(workspaceCtx.close)

	gotConn, err := workspaceCtx.getWorkspaceConn(ctx)
	require.NoError(t, err)
	require.Same(t, conn, gotConn)
	require.Equal(t, []uuid.UUID{staleAgentID, currentAgentID}, dialed)
	require.Equal(t, updatedChat, currentChat)

	gotAgent, err := workspaceCtx.getWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, currentAgent, gotAgent)
}

func TestTurnWorkspaceContext_SelectWorkspaceClearsCachedState(t *testing.T) {
	t.Parallel()

	ctrl := gomock.NewController(t)
	currentChat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  uuid.New(),
			Valid: true,
		},
	}
	updatedChat := database.Chat{
		ID: currentChat.ID,
		WorkspaceID: uuid.NullUUID{
			UUID:  uuid.New(),
			Valid: true,
		},
	}
	cachedConn := agentconnmock.NewMockAgentConn(ctrl)
	releaseCalls := 0

	workspaceCtx := turnWorkspaceContext{
		chatStateMu: &sync.Mutex{},
		currentChat: &currentChat,
	}
	workspaceCtx.agent = database.WorkspaceAgent{ID: uuid.New()}
	workspaceCtx.agentLoaded = true
	workspaceCtx.conn = cachedConn
	workspaceCtx.cachedWorkspaceID = currentChat.WorkspaceID
	workspaceCtx.releaseConn = func() {
		releaseCalls++
	}

	workspaceCtx.selectWorkspace(updatedChat)

	require.Equal(t, updatedChat, currentChat)
	require.Equal(t, 1, releaseCalls)

	workspaceCtx.mu.Lock()
	defer workspaceCtx.mu.Unlock()
	require.Equal(t, database.WorkspaceAgent{}, workspaceCtx.agent)
	require.False(t, workspaceCtx.agentLoaded)
	require.Nil(t, workspaceCtx.conn)
	require.Nil(t, workspaceCtx.releaseConn)
	require.Equal(t, uuid.NullUUID{}, workspaceCtx.cachedWorkspaceID)
}

func TestTurnWorkspaceContext_EnsureWorkspaceAgentIgnoresCachedAgentForDifferentWorkspace(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceOneID := uuid.New()
	workspaceTwoID := uuid.New()
	buildID := uuid.New()
	cachedAgent := database.WorkspaceAgent{ID: uuid.New()}
	resolvedAgent := database.WorkspaceAgent{ID: uuid.New()}
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceTwoID,
			Valid: true,
		},
	}
	updatedChat := chat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: resolvedAgent.ID, Valid: true}

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return([]database.WorkspaceAgent{resolvedAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return(database.WorkspaceBuild{ID: buildID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ID:                  chat.ID,
			ExpectedWorkspaceID: workspaceTwoID,
			BuildID:             uuid.NullUUID{UUID: buildID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: resolvedAgent.ID, Valid: true},
		}).Return(updatedChat, nil),
	)

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:           &Server{db: db},
		chatStateMu:      chatStateMu,
		currentChat:      &currentChat,
		loadChatSnapshot: func(context.Context, uuid.UUID) (database.Chat, error) { return database.Chat{}, nil },
	}
	workspaceCtx.agent = cachedAgent
	workspaceCtx.agentLoaded = true
	workspaceCtx.cachedWorkspaceID = uuid.NullUUID{UUID: workspaceOneID, Valid: true}
	defer workspaceCtx.close()

	chatSnapshot, agent, err := workspaceCtx.ensureWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, updatedChat, chatSnapshot)
	require.Equal(t, resolvedAgent, agent)
	require.Equal(t, updatedChat, currentChat)
}

func TestTurnWorkspaceContext_LoadWorkspaceAgentRetriesWhenWorkspaceChangesDuringPersist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceOneID := uuid.New()
	workspaceTwoID := uuid.New()
	buildOneID := uuid.New()
	buildTwoID := uuid.New()
	agentOne := database.WorkspaceAgent{ID: uuid.New()}
	agentTwo := database.WorkspaceAgent{ID: uuid.New()}
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceOneID,
			Valid: true,
		},
	}
	reloadedChat := database.Chat{
		ID: chat.ID,
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceTwoID,
			Valid: true,
		},
	}
	updatedChat := reloadedChat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildTwoID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: agentTwo.ID, Valid: true}
	loadCalls := 0

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceOneID).Return([]database.WorkspaceAgent{agentOne}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceOneID).Return(database.WorkspaceBuild{ID: buildOneID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ID:                  chat.ID,
			ExpectedWorkspaceID: workspaceOneID,
			BuildID:             uuid.NullUUID{UUID: buildOneID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: agentOne.ID, Valid: true},
		}).Return(database.Chat{}, sql.ErrNoRows),
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return([]database.WorkspaceAgent{agentTwo}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return(database.WorkspaceBuild{ID: buildTwoID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ID:                  chat.ID,
			ExpectedWorkspaceID: workspaceTwoID,
			BuildID:             uuid.NullUUID{UUID: buildTwoID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: agentTwo.ID, Valid: true},
		}).Return(updatedChat, nil),
	)

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:      &Server{db: db},
		chatStateMu: chatStateMu,
		currentChat: &currentChat,
		loadChatSnapshot: func(_ context.Context, id uuid.UUID) (database.Chat, error) {
			loadCalls++
			require.Equal(t, chat.ID, id)
			return reloadedChat, nil
		},
	}
	defer workspaceCtx.close()

	chatSnapshot, agent, err := workspaceCtx.ensureWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, updatedChat, chatSnapshot)
	require.Equal(t, agentTwo, agent)
	require.Equal(t, updatedChat, currentChat)
	require.Equal(t, 1, loadCalls)
}

func TestTurnWorkspaceContextGetWorkspaceConnRetriesWhenWorkspaceChangesDuringSwitchPersist(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	workspaceOneID := uuid.New()
	workspaceTwoID := uuid.New()
	staleAgentID := uuid.New()
	workspaceOneAgentID := uuid.New()
	workspaceTwoAgentID := uuid.New()
	buildOneID := uuid.New()
	buildTwoID := uuid.New()
	chat := database.Chat{
		ID: uuid.New(),
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceOneID,
			Valid: true,
		},
		AgentID: uuid.NullUUID{
			UUID:  staleAgentID,
			Valid: true,
		},
	}
	staleAgent := database.WorkspaceAgent{ID: staleAgentID}
	workspaceOneAgent := database.WorkspaceAgent{ID: workspaceOneAgentID}
	workspaceTwoAgent := database.WorkspaceAgent{ID: workspaceTwoAgentID}
	reloadedChat := database.Chat{
		ID: chat.ID,
		WorkspaceID: uuid.NullUUID{
			UUID:  workspaceTwoID,
			Valid: true,
		},
	}
	updatedChat := reloadedChat
	updatedChat.BuildID = uuid.NullUUID{UUID: buildTwoID, Valid: true}
	updatedChat.AgentID = uuid.NullUUID{UUID: workspaceTwoAgentID, Valid: true}
	loadCalls := 0
	discardedReleaseCalls := 0
	finalReleaseCalls := 0

	gomock.InOrder(
		db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), staleAgentID).Return(staleAgent, nil),
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceOneID).Return([]database.WorkspaceAgent{workspaceOneAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceOneID).Return(database.WorkspaceBuild{ID: buildOneID}, nil),
		db.EXPECT().GetWorkspaceAgentByID(gomock.Any(), workspaceOneAgentID).Return(workspaceOneAgent, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ID:                  chat.ID,
			ExpectedWorkspaceID: workspaceOneID,
			BuildID:             uuid.NullUUID{UUID: buildOneID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: workspaceOneAgentID, Valid: true},
		}).Return(database.Chat{}, sql.ErrNoRows),
		db.EXPECT().GetWorkspaceAgentsInLatestBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return([]database.WorkspaceAgent{workspaceTwoAgent}, nil),
		db.EXPECT().GetLatestWorkspaceBuildByWorkspaceID(gomock.Any(), workspaceTwoID).Return(database.WorkspaceBuild{ID: buildTwoID}, nil),
		db.EXPECT().UpdateChatBuildAgentBindingIfWorkspaceMatches(gomock.Any(), database.UpdateChatBuildAgentBindingIfWorkspaceMatchesParams{
			ID:                  chat.ID,
			ExpectedWorkspaceID: workspaceTwoID,
			BuildID:             uuid.NullUUID{UUID: buildTwoID, Valid: true},
			AgentID:             uuid.NullUUID{UUID: workspaceTwoAgentID, Valid: true},
		}).Return(updatedChat, nil),
	)

	discardedConn := agentconnmock.NewMockAgentConn(ctrl)
	finalConn := agentconnmock.NewMockAgentConn(ctrl)
	finalConn.EXPECT().SetExtraHeaders(gomock.Any()).Times(1)

	server := &Server{db: db}
	server.agentConnFn = func(_ context.Context, agentID uuid.UUID) (workspacesdk.AgentConn, func(), error) {
		switch agentID {
		case staleAgentID:
			return nil, nil, xerrors.New("dial failed")
		case workspaceOneAgentID:
			return discardedConn, func() {
				discardedReleaseCalls++
			}, nil
		case workspaceTwoAgentID:
			return finalConn, func() {
				finalReleaseCalls++
			}, nil
		default:
			return nil, nil, xerrors.Errorf("unexpected agent ID %q", agentID)
		}
	}

	chatStateMu := &sync.Mutex{}
	currentChat := chat
	workspaceCtx := turnWorkspaceContext{
		server:      server,
		chatStateMu: chatStateMu,
		currentChat: &currentChat,
		loadChatSnapshot: func(_ context.Context, id uuid.UUID) (database.Chat, error) {
			loadCalls++
			require.Equal(t, chat.ID, id)
			return reloadedChat, nil
		},
	}

	gotConn, err := workspaceCtx.getWorkspaceConn(ctx)
	require.NoError(t, err)
	require.Same(t, finalConn, gotConn)
	require.Equal(t, updatedChat, currentChat)
	require.Equal(t, 1, loadCalls)
	require.Equal(t, 1, discardedReleaseCalls)
	require.Equal(t, 0, finalReleaseCalls)

	gotAgent, err := workspaceCtx.getWorkspaceAgent(ctx)
	require.NoError(t, err)
	require.Equal(t, workspaceTwoAgent, gotAgent)

	workspaceCtx.close()
	require.Equal(t, 1, finalReleaseCalls)
}

func TestSubscribeSkipsDatabaseCatchupForLocallyDeliveredMessage(t *testing.T) {
	t.Parallel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	chatID := uuid.New()
	chat := database.Chat{ID: chatID, Status: database.ChatStatusPending}
	initialMessage := database.ChatMessage{
		ID:     1,
		ChatID: chatID,
		Role:   database.ChatMessageRoleUser,
	}
	localMessage := database.ChatMessage{
		ID:     2,
		ChatID: chatID,
		Role:   database.ChatMessageRoleAssistant,
	}

	gomock.InOrder(
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 0,
		}).Return([]database.ChatMessage{initialMessage}, nil),
		db.EXPECT().GetChatQueuedMessages(gomock.Any(), chatID).Return(nil, nil),
		db.EXPECT().GetChatByID(gomock.Any(), chatID).Return(chat, nil),
	)

	server := newSubscribeTestServer(t, db)
	_, events, cancel, ok := server.Subscribe(ctx, chatID, nil, 0)
	require.True(t, ok)
	defer cancel()

	server.publishMessage(chatID, localMessage)

	event := requireStreamMessageEvent(t, events)
	require.Equal(t, int64(2), event.Message.ID)
	requireNoStreamEvent(t, events, 200*time.Millisecond)
}

func TestSubscribeUsesDurableCacheWhenLocalMessageWasNotDelivered(t *testing.T) {
	t.Parallel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	chatID := uuid.New()
	chat := database.Chat{ID: chatID, Status: database.ChatStatusPending}
	initialMessage := database.ChatMessage{
		ID:     1,
		ChatID: chatID,
		Role:   database.ChatMessageRoleUser,
	}
	cachedMessage := codersdk.ChatMessage{
		ID:     2,
		ChatID: chatID,
		Role:   codersdk.ChatMessageRoleAssistant,
	}

	gomock.InOrder(
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 0,
		}).Return([]database.ChatMessage{initialMessage}, nil),
		db.EXPECT().GetChatQueuedMessages(gomock.Any(), chatID).Return(nil, nil),
		db.EXPECT().GetChatByID(gomock.Any(), chatID).Return(chat, nil),
	)

	server := newSubscribeTestServer(t, db)
	server.cacheDurableMessage(chatID, codersdk.ChatStreamEvent{
		Type:    codersdk.ChatStreamEventTypeMessage,
		ChatID:  chatID,
		Message: &cachedMessage,
	})

	_, events, cancel, ok := server.Subscribe(ctx, chatID, nil, 0)
	require.True(t, ok)
	defer cancel()

	server.publishChatStreamNotify(chatID, coderdpubsub.ChatStreamNotifyMessage{
		AfterMessageID: 1,
	})

	event := requireStreamMessageEvent(t, events)
	require.Equal(t, int64(2), event.Message.ID)
	requireNoStreamEvent(t, events, 200*time.Millisecond)
}

func TestSubscribeQueriesDatabaseWhenDurableCacheMisses(t *testing.T) {
	t.Parallel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	chatID := uuid.New()
	chat := database.Chat{ID: chatID, Status: database.ChatStatusPending}
	initialMessage := database.ChatMessage{
		ID:     1,
		ChatID: chatID,
		Role:   database.ChatMessageRoleUser,
	}
	catchupMessage := database.ChatMessage{
		ID:     2,
		ChatID: chatID,
		Role:   database.ChatMessageRoleAssistant,
	}

	gomock.InOrder(
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 0,
		}).Return([]database.ChatMessage{initialMessage}, nil),
		db.EXPECT().GetChatQueuedMessages(gomock.Any(), chatID).Return(nil, nil),
		db.EXPECT().GetChatByID(gomock.Any(), chatID).Return(chat, nil),
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 1,
		}).Return([]database.ChatMessage{catchupMessage}, nil),
	)

	server := newSubscribeTestServer(t, db)
	_, events, cancel, ok := server.Subscribe(ctx, chatID, nil, 0)
	require.True(t, ok)
	defer cancel()

	server.publishChatStreamNotify(chatID, coderdpubsub.ChatStreamNotifyMessage{
		AfterMessageID: 1,
	})

	event := requireStreamMessageEvent(t, events)
	require.Equal(t, int64(2), event.Message.ID)
	requireNoStreamEvent(t, events, 200*time.Millisecond)
}

func TestSubscribeFullRefreshStillUsesDatabaseCatchup(t *testing.T) {
	t.Parallel()

	ctx, cancelCtx := context.WithCancel(context.Background())
	defer cancelCtx()

	ctrl := gomock.NewController(t)
	db := dbmock.NewMockStore(ctrl)

	chatID := uuid.New()
	chat := database.Chat{ID: chatID, Status: database.ChatStatusPending}
	initialMessage := database.ChatMessage{
		ID:     1,
		ChatID: chatID,
		Role:   database.ChatMessageRoleUser,
	}
	editedMessage := database.ChatMessage{
		ID:     1,
		ChatID: chatID,
		Role:   database.ChatMessageRoleUser,
	}

	gomock.InOrder(
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 0,
		}).Return([]database.ChatMessage{initialMessage}, nil),
		db.EXPECT().GetChatQueuedMessages(gomock.Any(), chatID).Return(nil, nil),
		db.EXPECT().GetChatByID(gomock.Any(), chatID).Return(chat, nil),
		db.EXPECT().GetChatMessagesByChatID(gomock.Any(), database.GetChatMessagesByChatIDParams{
			ChatID:  chatID,
			AfterID: 0,
		}).Return([]database.ChatMessage{editedMessage}, nil),
	)

	server := newSubscribeTestServer(t, db)
	_, events, cancel, ok := server.Subscribe(ctx, chatID, nil, 0)
	require.True(t, ok)
	defer cancel()

	server.publishEditedMessage(chatID, editedMessage)

	event := requireStreamMessageEvent(t, events)
	require.Equal(t, int64(1), event.Message.ID)
	requireNoStreamEvent(t, events, 200*time.Millisecond)
}

func newSubscribeTestServer(t *testing.T, db database.Store) *Server {
	t.Helper()

	return &Server{
		db:     db,
		logger: slogtest.Make(t, &slogtest.Options{IgnoreErrors: true}),
		pubsub: dbpubsub.NewInMemory(),
	}
}

func requireStreamMessageEvent(t *testing.T, events <-chan codersdk.ChatStreamEvent) codersdk.ChatStreamEvent {
	t.Helper()

	select {
	case event, ok := <-events:
		require.True(t, ok, "chat stream closed before delivering an event")
		require.Equal(t, codersdk.ChatStreamEventTypeMessage, event.Type)
		require.NotNil(t, event.Message)
		return event
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for chat stream message event")
		return codersdk.ChatStreamEvent{}
	}
}

func requireNoStreamEvent(t *testing.T, events <-chan codersdk.ChatStreamEvent, wait time.Duration) {
	t.Helper()

	select {
	case event, ok := <-events:
		if !ok {
			t.Fatal("chat stream closed unexpectedly")
		}
		t.Fatalf("unexpected chat stream event: %+v", event)
	case <-time.After(wait):
	}
}
