package duality

import (
	"context"
	"fmt"
	"testing"

	dextypes "github.com/NicholasDotSol/duality/x/dex/types"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	simappparams "github.com/cosmos/cosmos-sdk/simapp/params"
	"github.com/strangelove-ventures/ibctest/v3"
	"github.com/strangelove-ventures/ibctest/v3/chain/cosmos"
	"github.com/strangelove-ventures/ibctest/v3/ibc"
	"github.com/strangelove-ventures/ibctest/v3/internal/dockerutil"
	"github.com/strangelove-ventures/ibctest/v3/test"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

const (
	aliceKeyName        = "alice"
	bobKeyName          = "bob"
	rlyDualityKeyName   = "relayer-duality"
	rlyGaiaMnemonic     = "turkey sustain spoil ostrich false cradle tackle silent collect maple walnut brave rookie melody busy float monkey large drama romance rib search ride diary"
	gaiaUserMnemonic    = "obscure reform almost timber anxiety wave use shield choose icon crack visual bunker mountain wild range child cross wedding organ make tube oxygen talent"
	genesisWalletAmount = int64(100_000_000)
	pathGaiaDuality     = "gaia-duality"
	ibcTransferAmount   = int64(100_000)
)

var (
	chainCfg = ibc.ChainConfig{
		Type:    "cosmos",
		Name:    "duality",
		ChainID: "duality-1",
		Images: []ibc.DockerImage{{
			Repository: "duality",
			Version:    "local",
			UidGid:     dockerutil.GetHeighlinerUserString(),
		}},
		Bin:                 "dualityd",
		Bech32Prefix:        "cosmos",
		Denom:               "stake",
		GasPrices:           "0.0stake",
		GasAdjustment:       1.2,
		TrustingPeriod:      "336h",
		NoHostMount:         false,
		ModifyGenesis:       nil,
		ConfigFileOverrides: nil,
		EncodingConfig:      dualityEncoding(),
	}
)

// dualityEncoding registers the Duality dex modules custom types, so we can see them in the block database.
func dualityEncoding() *simappparams.EncodingConfig {
	cfg := cosmos.DefaultEncoding()
	dextypes.RegisterInterfaces(cfg.InterfaceRegistry)
	return &cfg
}

// TestDualityConsumerChainStart asserts that the chain can be properly spun up as a standalone consumer chain.
func TestDualityConsumerChainStart(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	// Number of full nodes and validators in the network
	nv := 1
	nf := 0

	// Create chain factory with Duality
	cf := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{Name: "duality", ChainConfig: chainCfg, NumValidators: &nv, NumFullNodes: &nf}},
	)

	// Get chain from the chain factory
	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)
	duality := chains[0].(*cosmos.CosmosChain)

	ctx := context.Background()
	client, network := ibctest.DockerSetup(t)

	// Initialize the Duality nodes
	err = duality.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize duality chain")

	dualityValidator := duality.Validators[0]

	// Initialize the Duality node files, create genesis wallets, and start the chain
	kr := keyring.NewInMemory()

	dualityWallets, err := initDuality(ctx, dualityValidator, kr, []string{aliceKeyName})
	require.NoError(t, err)

	t.Cleanup(func() {
		err = dualityValidator.StopContainer(ctx)
		if err != nil {
			panic(fmt.Errorf("failed to stop duality validator container: %w", err))
		}
	})

	// Wait a block to ensure the chain is up and running
	err = test.WaitForBlocks(ctx, 1, duality)
	require.NoError(t, err)

	// Assert that the genesis wallet contains the specified balance from initialization
	// Mostly just here to ensure we can now query state from the chain
	bal, err := duality.GetBalance(ctx, dualityWallets[0].Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, genesisWalletAmount, bal)
}
