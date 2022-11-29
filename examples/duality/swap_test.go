package duality

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"testing"

	"github.com/NicholasDotSol/duality/x/dex/types"
	swaptypes "github.com/NicholasDotSol/duality/x/ibc-swap/types"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdktypes "github.com/cosmos/cosmos-sdk/types"
	transfertypes "github.com/cosmos/ibc-go/v3/modules/apps/transfer/types"
	"github.com/icza/dyno"
	"github.com/strangelove-ventures/ibctest/v3"
	"github.com/strangelove-ventures/ibctest/v3/chain/cosmos"
	"github.com/strangelove-ventures/ibctest/v3/ibc"
	"github.com/strangelove-ventures/ibctest/v3/relayer"
	"github.com/strangelove-ventures/ibctest/v3/relayer/rly"
	"github.com/strangelove-ventures/ibctest/v3/test"
	"github.com/strangelove-ventures/ibctest/v3/testreporter"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
)

// TestDualityIBCSwapMiddleware asserts that the IBC swap middleware works as intended with Duality running as a
// standalone consumer chain connected to the Cosmos Hub.
func TestDualityIBCSwapMiddleware(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}

	t.Parallel()

	// Number of full nodes and validators in the network
	nv := 1
	nf := 0

	// Create chain factory with Gaia and Duality
	cf := ibctest.NewBuiltinChainFactory(zaptest.NewLogger(t), []*ibctest.ChainSpec{
		{Name: "gaia", Version: "strangelove-forward_middleware_memo_v3", ChainConfig: ibc.ChainConfig{ChainID: "cosmoshub-4", GasPrices: "0.0uatom"}},
		{Name: "duality", ChainConfig: chainCfg, NumValidators: &nv, NumFullNodes: &nf}},
	)

	// Get both chains from the chain factory
	chains, err := cf.Chains(t.Name())
	require.NoError(t, err)
	gaia, duality := chains[0].(*cosmos.CosmosChain), chains[1].(*cosmos.CosmosChain)

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

	// Wait a few blocks to ensure the wallets are created and funded
	err = test.WaitForBlocks(ctx, 5, gaia)
	require.NoError(t, err)

	// Get our bech32 encoded user address
	gaiaAddr := gaiaKey.Bech32Address(gaia.Config().Bech32Prefix)

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

	// Configure keys for the relayer to use for both chains
	err = r.RestoreKey(ctx, eRep, gaia.Config().ChainID, rlyGaiaKey.KeyName, rlyGaiaKey.Mnemonic)
	require.NoError(t, err)

	err = r.RestoreKey(ctx, eRep, duality.Config().ChainID, rlyDualityKey.KeyName, rlyDualityKey.Mnemonic)
	require.NoError(t, err)

	// Create a new path in the relayer config for the Gaia<>Duality path
	err = r.GeneratePath(ctx, eRep, gaia.Config().ChainID, duality.Config().ChainID, pathGaiaDuality)
	require.NoError(t, err)

	// Link the path between Gaia and Duality
	err = r.LinkPath(ctx, eRep, pathGaiaDuality, ibc.DefaultChannelOpts(), ibc.CreateClientOptions{TrustingPeriod: "330h"})
	require.NoError(t, err)

	// Start the relayer
	require.NoError(t, r.StartRelayer(ctx, eRep, pathGaiaDuality))

	t.Cleanup(
		func() {
			err := r.StopRelayer(ctx, eRep)
			if err != nil {
				panic(fmt.Errorf("an error occured while stopping the relayer: %s", err))
			}
		},
	)

	// Get channel between Gaia and Duality
	channels, err := r.GetChannels(ctx, eRep, gaia.Config().ChainID)
	require.NoError(t, err)
	require.Equal(t, 1, len(channels))
	channel := channels[0]

	// Compose details for an IBC transfer
	transfer := ibc.WalletAmount{
		Address: dualityKey.Address,
		Denom:   gaia.Config().Denom,
		Amount:  ibcTransferAmount,
	}

	// Send an IBC transfer from Gaia to Duality, so we can initialize a pool with the IBC denom token + native Duality token
	transferTx, err := gaia.SendIBCTransfer(ctx, channel.ChannelID, gaiaAddr, transfer, ibc.TransferOptions{
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
	gaiaTokenDenom := transfertypes.GetPrefixedDenom(channel.Counterparty.PortID, channel.Counterparty.ChannelID, gaia.Config().Denom)
	gaiaDenomTrace := transfertypes.ParseDenomTrace(gaiaTokenDenom)

	// Assert that the funds are gone from the acc on Gaia and present in the acc on Duality
	gaiaBalTransfer, err := gaia.GetBalance(ctx, gaiaAddr, gaia.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, gaiaOrigBalNative-ibcTransferAmount, gaiaBalTransfer)

	dualityBalIBCTransfer, err := duality.GetBalance(ctx, dualityKey.Address, gaiaDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, ibcTransferAmount, dualityBalIBCTransfer)

	// dualityd tx dex deposit [receiver] [token-a] [token-b] [list of amount-0] [list of amount-1] [list of tick-index] [list of fee] [flags]
	depositAmount, err := sdktypes.NewDecFromStr(strconv.FormatInt(100000, 10))
	require.NoError(t, err)

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
	require.Equal(t, dualityBalIBCTransfer-depositAmount.RoundInt64(), dualityBalIBC)

	dualityBalNative, err := duality.GetBalance(ctx, dualityKey.Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, dualityOrigBalNative-depositAmount.RoundInt64(), dualityBalNative)

	// --- Begin the IBC transfer with the swap

	swapAmount, err := sdktypes.NewDecFromStr(strconv.FormatInt(100000, 10))
	require.NoError(t, err)

	minOut, err := sdktypes.NewDecFromStr(strconv.FormatInt(100000, 10))
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
			Next: "",
		},
	}

	metadataBz, err := json.Marshal(metadata)
	require.NoError(t, err)

	gaiaHeight, err = gaia.Height(ctx)
	require.NoError(t, err)

	// Send an IBC transfer from Gaia to Duality with packet memo containing the swap metadata
	transferTx, err = gaia.SendIBCTransfer(ctx, channel.ChannelID, gaiaAddr, transfer, ibc.TransferOptions{Memo: string(metadataBz)})
	require.NoError(t, err)

	// Poll for the ack to know that the swap is complete
	_, err = test.PollForAck(ctx, gaia, gaiaHeight, gaiaHeight+10, transferTx.Packet)
	require.NoError(t, err)

	// Check that the funds are moved out of the acc on Gaia
	gaiaBalAfterSwap, err := gaia.GetBalance(ctx, gaiaAddr, gaia.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, gaiaBalTransfer-ibcTransferAmount, gaiaBalAfterSwap)

	// Check that the funds are now present in the acc on Duality
	dualityBalNativeSwap, err := duality.GetBalance(ctx, dualityKey.Address, duality.Config().Denom)
	require.NoError(t, err)
	require.Equal(t, dualityBalNative+minOut.RoundInt64(), dualityBalNativeSwap)

	dualityBalIBCSwap, err := duality.GetBalance(ctx, dualityKey.Address, gaiaDenomTrace.IBCDenom())
	require.NoError(t, err)
	require.Equal(t, dualityBalIBC, dualityBalIBCSwap)
}

// initDuality creates and funds the genesis wallets, initializes the nodes, adds the standalone consumer chain
// data to the genesis file and starts the chain.
func initDuality(
	ctx context.Context,
	dualityValidator *cosmos.ChainNode,
	kr keyring.Keyring,
	keys []string,
) ([]ibc.Wallet, error) {
	userWallets := make([]ibc.Wallet, len(keys))

	// Generate wallet mnemonics and add to the keyring
	for i, key := range keys {
		wallet := ibctest.BuildWallet(kr, key, chainCfg)
		wallet.KeyName = key
		userWallets[i] = wallet

		err := dualityValidator.RecoverKey(ctx, key, wallet.Mnemonic)
		if err != nil {
			return nil, fmt.Errorf("failed to restore key for %s: %w", key, err)
		}
	}

	// Initialize the nodes on Duality
	err := dualityValidator.InitFullNodeFiles(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize duality validator config")
	}

	// Generate initial wallet balances to be used at genesis
	genesisWallets := make([]ibc.WalletAmount, len(userWallets))
	for i, wallet := range userWallets {
		genesisWallets[i] = ibc.WalletAmount{
			Address: wallet.Address,
			Denom:   chainCfg.Denom,
			Amount:  genesisWalletAmount,
		}
	}

	// Add genesis accounts for each wallet
	for _, wallet := range genesisWallets {
		err = dualityValidator.AddGenesisAccount(ctx, wallet.Address, []sdktypes.Coin{sdktypes.NewCoin(wallet.Denom, sdktypes.NewIntFromUint64(uint64(wallet.Amount)))})
		if err != nil {
			return nil, fmt.Errorf("failed to add genesis account for %s: %w", wallet.Address, err)
		}
	}

	// Read the genesis file, modify it, and then overwrite the genesis file on the node
	genBz, err := dualityValidator.GenesisFileContent(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to read duality genesis file: %w", err)
	}

	feeList := Fees{FeeList: []FeeList{
		{0, 1},
		{Id: 1, Fee: 0},
		{Id: 2, Fee: 5},
		{Id: 3, Fee: 10},
	}}

	genBz, err = modifyGenesisDuality(genBz, feeList)
	if err != nil {
		return nil, fmt.Errorf("failed to modify duality genesis file: %w", err)
	}

	err = dualityValidator.OverwriteGenesisFile(ctx, genBz)
	if err != nil {
		return nil, fmt.Errorf("failed to write duality genesis file: %w", err)
	}

	// Execute the command to alter the genesis file for Duality to run as a standalone consumer chain
	_, _, err = dualityValidator.ExecBin(ctx, "add-consumer-section")
	if err != nil {
		return nil, fmt.Errorf("failed to add consumer section to duality validator genesis file: %w", err)
	}

	// Create and start the container for the single validator on Duality
	err = dualityValidator.CreateNodeContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create duality validator container: %w", err)
	}

	err = dualityValidator.StartContainer(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create duality validator container: %w", err)
	}

	return userWallets, nil
}

func modifyGenesisDuality(genbz []byte, feeList Fees) ([]byte, error) {
	g := make(map[string]interface{})
	if err := json.Unmarshal(genbz, &g); err != nil {
		return nil, fmt.Errorf("failed to unmarshal genesis file: %w", err)
	}
	if err := dyno.Set(g, feeList.FeeList, "app_state", "dex", "feeListList"); err != nil {
		return nil, fmt.Errorf("failed to set fee list in genesis json: %w", err)
	}
	if err := dyno.Set(g, len(feeList.FeeList), "app_state", "dex", "feeListCount"); err != nil {
		return nil, fmt.Errorf("failed set fee list count in genesis json")
	}

	out, err := json.Marshal(g)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal genesis bytes to json: %w", err)
	}
	return out, nil

}

type Fees struct {
	FeeList []FeeList `yaml:"feeListList"`
}

type FeeList struct {
	Id  uint64 `protobuf:"varint,1,opt,name=id,proto3" json:"id,omitempty"`
	Fee int64  `protobuf:"varint,2,opt,name=fee,proto3" json:"fee,omitempty"`
}
