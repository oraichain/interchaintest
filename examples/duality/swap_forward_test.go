package duality

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/NicholasDotSol/duality/x/dex/types"
	swaptypes "github.com/NicholasDotSol/duality/x/ibc-swap/types"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdktypes "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	"github.com/strangelove-ventures/ibctest/v3"
	"github.com/strangelove-ventures/ibctest/v3/chain/cosmos"
	"github.com/strangelove-ventures/ibctest/v3/ibc"
	"github.com/strangelove-ventures/ibctest/v3/relayer"
	"github.com/strangelove-ventures/ibctest/v3/relayer/rly"
	"github.com/strangelove-ventures/ibctest/v3/test"
	"github.com/strangelove-ventures/ibctest/v3/testreporter"
	forwardtypes "github.com/strangelove-ventures/packet-forward-middleware/v3/router/types"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// TestDualitySwapAndForward asserts that the swap and forward middleware stack works as intended with Duality running as a
// standalone consumer chain connected to the Cosmos Hub and Osmosis.
func TestDualitySwapAndForward(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	// Number of full nodes and validators in the network
	nv := 1
	nf := 0

	// Create chain factory with Gaia and Duality
	cf := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{Name: "gaia", Version: "strangelove-forward_middleware_memo_v3", ChainConfig: ibc.ChainConfig{ChainID: "cosmoshub-1", GasPrices: "0.0uatom"}},
		{Name: "duality", ChainConfig: chainCfg, NumValidators: &nv, NumFullNodes: &nf},
		{Name: "gaia", Version: "strangelove-forward_middleware_memo_v3", ChainConfig: ibc.ChainConfig{ChainID: "cosmoshub-2", GasPrices: "0.0uatom"}}},
	)

	// Get both chains from the chain factory
	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)
	gaia, duality, chainC := chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain), chains[2].(*cosmos.CosmosChain)

	ctx := context.Background()
	client, network := ibctest.DockerSetup(t)

	// Create relayer factory with the go-relayer
	// TODO the custom docker image can be removed here once ICS query fix is merged into main in the relayer
	r := ibctest.NewBuiltinRelayerFactory(
		ibc.CosmosRly,
		zaptest.NewLogger(t),
		relayer.CustomDockerImage("ghcr.io/cosmos/relayer", "andrew-ics_consumer_unbonding_period_query", rly.RlyDefaultUidGid),
	).Build(t, client, network)

	// Initialize the Interchain object which describes the chains, relayers, and paths between chains
	// We only use this for spinning up Gaia and initializing the relayer config because there is no ICS support for Duality.
	ic := ibctest.NewInterchain().
		AddChain(gaia).
		AddChain(chainC).
		AddRelayer(r, "relayer")

	rep := testreporter.NewNopReporter()
	eRep := rep.RelayerExecReporter(t)

	require.NoError(t, ic.Build(ctx, eRep, ibctest.InterchainBuildOptions{
		TestName:          t.Name(),
		Client:            client,
		NetworkID:         network,
		BlockDatabaseFile: ibctest.DefaultBlockDatabaseFilepath(),

		SkipPathCreation: true,
	}))

	t.Cleanup(func() {
		_ = ic.Close()
	})

	// Initialize the Duality nodes
	err = duality.Initialize(ctx, t.Name(), client, network)
	require.NoError(t, err, "failed to initialize duality chain")

	dualityValidator := duality.Validators[0]

	// Initialize the Duality node files, create genesis wallets, and start the chain
	kr := keyring.NewInMemory()

	dualityWallets, err := initDuality(ctx, dualityValidator, kr, []string{aliceKeyName, rlyDualityKeyName})
	require.NoError(t, err)

	dualityKey, rlyDualityKey := dualityWallets[0], dualityWallets[1]

	t.Cleanup(func() {
		err = dualityValidator.StopContainer(ctx)
		if err != nil {
			panic(fmt.Errorf("failed to stop duality validator container: %w", err))
		}
	})

	// Create and fund a wallet on Gaia for the relayer and a user acc
	gaiaKey, err := ibctest.GetAndFundTestUserWithMnemonic(ctx, t.Name(), gaiaUserMnemonic, genesisWalletAmount, gaia)
	require.NoError(t, err)
	gaiaKey.Mnemonic = gaiaUserMnemonic

	rlyGaiaKey, err := ibctest.GetAndFundTestUserWithMnemonic(ctx, t.Name(), rlyGaiaMnemonic, genesisWalletAmount, gaia)
	require.NoError(t, err)
	rlyGaiaKey.Mnemonic = rlyGaiaMnemonic

	osmosisKey, err := ibctest.GetAndFundTestUserWithMnemonic(ctx, t.Name(), osmosisUserMnemonic, genesisWalletAmount, chainC)
	require.NoError(t, err)
	osmosisKey.Mnemonic = osmosisUserMnemonic

	rlyOsmosisKey, err := ibctest.GetAndFundTestUserWithMnemonic(ctx, t.Name(), rlyOsmosisMnemonic, genesisWalletAmount, chainC)
	require.NoError(t, err)
	rlyOsmosisKey.Mnemonic = rlyOsmosisMnemonic

	// Wait a few blocks to ensure the wallets are created and funded
	err = test.WaitForBlocks(ctx, 5, gaia)
	require.NoError(t, err)

	// Get our bech32 encoded user address
	gaiaAddr := gaiaKey.Bech32Address(gaia.Config().Bech32Prefix)
	osmosisAddr := osmosisKey.Bech32Address(chainC.Config().Bech32Prefix)

	// Get the original acc balances on both chains for their native tokens
	gaiaOrigBalNative, err := gaia.GetBalance(ctx, gaiaAddr, gaia.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, genesisWalletAmount, gaiaOrigBalNative)

	dualityOrigBalNative, err := duality.GetBalance(ctx, dualityKey.Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, genesisWalletAmount, dualityOrigBalNative)

	// Add chain configs to the relayer for both chains
	err = r.AddChainConfiguration(ctx, eRep, gaia.Config(), rlyGaiaKey.KeyName, gaia.GetRPCAddress(), gaia.GetGRPCAddress())
	require.NoError(t, err)

	err = r.AddChainConfiguration(ctx, eRep, duality.Config(), rlyDualityKey.KeyName, duality.GetRPCAddress(), duality.GetGRPCAddress())
	require.NoError(t, err)

	err = r.AddChainConfiguration(ctx, eRep, chainC.Config(), rlyOsmosisKey.KeyName, chainC.GetRPCAddress(), chainC.GetGRPCAddress())
	require.NoError(t, err)

	// Configure keys for the relayer to use for both chains
	err = r.RestoreKey(ctx, eRep, gaia.Config().ChainID, rlyGaiaKey.KeyName, rlyGaiaKey.Mnemonic)
	require.NoError(t, err)

	err = r.RestoreKey(ctx, eRep, duality.Config().ChainID, rlyDualityKey.KeyName, rlyDualityKey.Mnemonic)
	require.NoError(t, err)

	err = r.RestoreKey(ctx, eRep, chainC.Config().ChainID, rlyOsmosisKey.KeyName, rlyOsmosisKey.Mnemonic)
	require.NoError(t, err)

	// Create a new path in the relayer config for the Gaia<>Duality path
	err = r.GeneratePath(ctx, eRep, gaia.Config().ChainID, duality.Config().ChainID, pathGaiaDuality)
	require.NoError(t, err)

	err = r.GeneratePath(ctx, eRep, chainC.Config().ChainID, duality.Config().ChainID, pathDualityOsmosis)
	require.NoError(t, err)

	// Link the path between Gaia and Duality
	err = r.LinkPath(ctx, eRep, pathGaiaDuality, ibc.DefaultChannelOpts(), ibc.CreateClientOptions{TrustingPeriod: "330h"})
	require.NoError(t, err)

	err = r.LinkPath(ctx, eRep, pathDualityOsmosis, ibc.DefaultChannelOpts(), ibc.CreateClientOptions{TrustingPeriod: "330h"})
	require.NoError(t, err)

	// Start the relayer
	require.NoError(t, r.StartRelayer(ctx, eRep, pathGaiaDuality, pathDualityOsmosis))

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				panic(fmt.Errorf("an error occured while stopping the relayer: %s", err))
			}
		},
	)

	// Get channel between Gaia and Duality
	gaiaChannels, err := r.GetChannels(ctx, eRep, gaia.Config().ChainID)
	require.NoError(t, err)
	require.Equal(t, 1, len(gaiaChannels))
	gaiaChannel := gaiaChannels[0]

	osmosisChannels, err := r.GetChannels(ctx, eRep, chainC.Config().ChainID)
	require.NoError(t, err)
	require.Equal(t, 1, len(osmosisChannels))
	osmosisChannel := osmosisChannels[0]

	// Compose details for an IBC transfer
	transfer := ibc.WalletAmount{
		Address: dualityKey.Address,
		Denom:   gaia.Config().Denom,
		Amount:  ibcTransferAmount,
	}

	// Send an IBC transfer from Gaia to Duality, so we can initialize a pool with the IBC denom token + native Duality token
	transferTx, err := gaia.SendIBCTransfer(ctx, gaiaChannel.ChannelID, gaiaAddr, transfer, ibc.TransferOptions{
		Timeout: nil,
		Memo:    "",
	})
	require.NoError(t, err)

	gaiaHeight, err := gaia.Height(ctx)
	require.NoError(t, err)

	// Poll for the ack to know that the transfer is complete
	_, err = test.PollForAck(ctx, gaia, gaiaHeight, gaiaHeight+10, transferTx.Packet)
	require.NoError(t, err)

	// Get the IBC denom for ATOM on Duality
	gaiaTokenDenom := transfertypes.GetPrefixedDenom(gaiaChannel.Counterparty.PortID, gaiaChannel.Counterparty.ChannelID, gaia.Config().Denom)
	gaiaDenomTrace := transfertypes.ParseDenomTrace(gaiaTokenDenom)

	// Get the IBC denom for ATOM on Osmosis which has moved through Duality
	osmosisTokenDenom := transfertypes.GetPrefixedDenom(osmosisChannel.PortID, osmosisChannel.ChannelID, duality.Config().Denom)
	osmosisDenomTrace := transfertypes.ParseDenomTrace(osmosisTokenDenom)

	// Assert that the funds are gone from the acc on Gaia and present in the acc on Duality
	gaiaBalTransfer, err := gaia.GetBalance(ctx, gaiaAddr, gaia.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, gaiaOrigBalNative-ibcTransferAmount, gaiaBalTransfer)

	dualityBalIBCTransfer, err := duality.GetBalance(ctx, dualityKey.Address, gaiaDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, ibcTransferAmount, dualityBalIBCTransfer)

	// dualityd tx dex deposit [receiver] [token-a] [token-b] [list of amount-0] [list of amount-1] [list of tick-index] [list of fee] [flags]
	depositAmount := sdktypes.NewInt(100000)

	depositCmd := []string{
		duality.Config().Bin, "tx", "dex", "deposit",
		dualityKey.Address,
		duality.Config().Denom,
		gaiaDenomTrace.IBCDenom(),
		depositAmount.String(),
		depositAmount.String(),
		"0",
		"1",
		"--chain-id", duality.Config().ChainID,
		"--node", duality.GetRPCAddress(),
		"--from", dualityKey.KeyName,
		"--keyring-backend", "test",
		"--gas", "auto",
		"--yes",
		"--home", duality.HomeDir(),
	}

	// Execute the deposit cmd to initialize the pool on Duality
	_, _, err = duality.Exec(ctx, depositCmd, nil)
	require.NoError(t, err)

	// Wait for the tx to be included in a block
	err = test.WaitForBlocks(ctx, 5, duality)
	require.NoError(t, err)

	// Assert that the deposit was successful and the funds are moved out of the Duality user acc
	dualityBalIBC, err := duality.GetBalance(ctx, dualityKey.Address, gaiaDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, dualityBalIBCTransfer-depositAmount.Int64(), dualityBalIBC)

	dualityBalNative, err := duality.GetBalance(ctx, dualityKey.Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, dualityOrigBalNative-depositAmount.Int64(), dualityBalNative)

	// --- Begin the IBC transfer with the swap

	swapAmount := sdktypes.NewInt(100000)
	minOut := sdktypes.NewInt(100000)

	retries := uint8(0)
	forwardMetadata := forwardtypes.PacketMetadata{
		Forward: &forwardtypes.ForwardMetadata{
			Receiver: osmosisAddr,
			Port:     osmosisChannel.Counterparty.PortID,
			Channel:  osmosisChannel.Counterparty.ChannelID,
			Timeout:  5 * time.Minute,
			Retries:  &retries,
			Next:     nil,
		}}

	bz, err := json.Marshal(forwardMetadata)
	require.NoError(t, err)

	metadata := swaptypes.PacketMetadata{
		Swap: &swaptypes.SwapMetadata{
			MsgSwap: &types.MsgSwap{
				Creator:  dualityKey.Address,
				Receiver: dualityKey.Address,
				TokenA:   gaiaDenomTrace.IBCDenom(),
				TokenB:   duality.Config().Denom,
				AmountIn: swapAmount,
				TokenIn:  gaiaDenomTrace.IBCDenom(),
				MinOut:   minOut,
			},
			Next: string(bz),
		},
	}

	metadataBz, err := json.Marshal(metadata)
	require.NoError(t, err)

	gaiaHeight, err = gaia.Height(ctx)
	require.NoError(t, err)

	// Send an IBC transfer from Gaia to Duality with packet memo containing the swap metadata
	transferTx, err = gaia.SendIBCTransfer(ctx, gaiaChannel.ChannelID, gaiaAddr, transfer, ibc.TransferOptions{Memo: string(metadataBz)})
	require.NoError(t, err)

	// Poll for the ack to know that the swap is complete
	_, err = test.PollForAck(ctx, gaia, gaiaHeight, gaiaHeight+15, transferTx.Packet)
	require.NoError(t, err)

	// Check that the funds are moved out of the acc on Gaia
	gaiaBalAfterSwap, err := gaia.GetBalance(ctx, gaiaAddr, gaia.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, gaiaBalTransfer-ibcTransferAmount, gaiaBalAfterSwap)

	require.NoError(t, test.WaitForBlocks(ctx, 20, duality, chainC))

	// Check that the funds are now present in the acc on Osmosis
	dualityBalNativeSwap, err := duality.GetBalance(ctx, dualityKey.Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, dualityBalNative, dualityBalNativeSwap)

	dualityBalIBCSwap, err := duality.GetBalance(ctx, dualityKey.Address, gaiaDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, dualityBalIBC, dualityBalIBCSwap)

	//t.Log("PAUSING...")
	//time.Sleep(5 * time.Minute)

	osmosisBal, err := chainC.GetBalance(ctx, osmosisAddr, osmosisDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, minOut.Int64(), osmosisBal)
}
