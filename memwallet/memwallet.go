package memwallet

import (
	"bytes"
	"github.com/jfixby/coinharness"
	"github.com/jfixby/dcrharness"
	"github.com/jfixby/pin"
	"sync"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrec/secp256k1"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/hdkeychain"
	"github.com/decred/dcrd/rpcclient"
	"github.com/decred/dcrd/wire"
)

// InMemoryWallet is a simple in-memory wallet whose purpose is to provide basic
// wallet functionality to the harness. The wallet uses a hard-coded HD key
// hierarchy which promotes reproducibility between harness test runs.
// Implements harness.TestWallet.
type InMemoryWallet struct {
	coinbaseKey  *secp256k1.PrivateKey
	coinbaseAddr dcrutil.Address

	// hdRoot is the root master private key for the wallet.
	hdRoot *hdkeychain.ExtendedKey

	// hdIndex is the next available key index offset from the hdRoot.
	hdIndex uint32

	// currentHeight is the latest height the wallet is known to be synced
	// to.
	currentHeight int64

	// addrs tracks all addresses belonging to the wallet. The addresses
	// are indexed by their keypath from the hdRoot.
	addrs map[uint32]dcrutil.Address

	// utxos is the set of utxos spendable by the wallet.
	utxos map[coinharness.OutPoint]*utxo

	// reorgJournal is a map storing an undo entry for each new block
	// received. Once a block is disconnected, the undo entry for the
	// particular height is evaluated, thereby rewinding the effect of the
	// disconnected block on the wallet's set of spendable utxos.
	reorgJournal map[int64]*undoEntry

	chainUpdates []*chainUpdate

	// chainUpdateSignal is a wallet event queue
	chainUpdateSignal chan string

	chainMtx sync.Mutex

	net coinharness.Network

	nodeRPC coinharness.RPCClient

	sync.RWMutex
	RPCClientFactory coinharness.RPCClientFactory

	NewTxFromBytes func(txBytes []byte) (*coinharness.Tx, error) //dcrutil.NewTxFromBytes(txBytes)
	IsCoinBaseTx   func(*coinharness.MessageTx) (bool)           //blockchain.IsCoinBaseTx(mtx)
}

// Network returns current network of the wallet
func (wallet *InMemoryWallet) Network() coinharness.Network {
	return wallet.net
}

// Start wallet process
func (wallet *InMemoryWallet) Start(args *coinharness.TestWalletStartArgs) error {
	handlers := &rpcclient.NotificationHandlers{}

	// If a handler for the OnBlockConnected/OnBlockDisconnected callback
	// has already been set, then we create a wrapper callback which
	// executes both the currently registered callback, and the mem
	// wallet's callback.
	if handlers.OnBlockConnected != nil {
		obc := handlers.OnBlockConnected
		handlers.OnBlockConnected = func(header []byte, filteredTxns [][]byte) {
			wallet.IngestBlock(header, filteredTxns)
			obc(header, filteredTxns)
		}
	} else {
		// Otherwise, we can claim the callback ourselves.
		handlers.OnBlockConnected = wallet.IngestBlock
	}
	if handlers.OnBlockDisconnected != nil {
		obd := handlers.OnBlockDisconnected
		handlers.OnBlockDisconnected = func(header []byte) {
			wallet.UnwindBlock(header)
			obd(header)
		}
	} else {
		handlers.OnBlockDisconnected = wallet.UnwindBlock
	}

	wallet.nodeRPC = coinharness.NewRPCConnection(wallet.RPCClientFactory, args.NodeRPCConfig, 5, handlers)
	pin.AssertNotNil("nodeRPC", wallet.nodeRPC)

	// Filter transactions that pay to the coinbase associated with the
	// wallet.
	wallet.updateTxFilter()

	// Ensure dcrd properly dispatches our registered call-back for each new
	// block. Otherwise, the InMemoryWallet won't function properly.
	err := wallet.nodeRPC.NotifyBlocks()
	pin.CheckTestSetupMalfunction(err)

	go wallet.chainSyncer()
	return nil
}

func (wallet *InMemoryWallet) updateTxFilter() {
	filterAddrs := []dcrutil.Address{}
	for _, v := range wallet.addrs {
		filterAddrs = append(filterAddrs, v)
	}
	err := wallet.nodeRPC.Internal().(*rpcclient.Client).LoadTxFilter(true, filterAddrs, nil)
	pin.CheckTestSetupMalfunction(err)
}

// Stop wallet process gently, by sending stopSignal to the wallet event queue
func (wallet *InMemoryWallet) Stop() {
	go func() {
		wallet.chainUpdateSignal <- stopSignal
	}()
	wallet.nodeRPC.Disconnect()
	wallet.nodeRPC = nil
}

// Sync block until the wallet has fully synced up to the tip of the main
// chain.
func (wallet *InMemoryWallet) Sync(desiredHeight int64) int64 {
	ticker := time.NewTicker(time.Millisecond * 100)
	for range ticker.C {
		walletHeight := wallet.SyncedHeight()
		if walletHeight >= desiredHeight {
			break
		}
	}
	return wallet.SyncedHeight()
}

// Dispose is no needed for InMemoryWallet
func (wallet *InMemoryWallet) Dispose() error {
	return nil
}

// SyncedHeight returns the height the wallet is known to be synced to.
//
// This function is safe for concurrent access.
func (wallet *InMemoryWallet) SyncedHeight() int64 {
	wallet.RLock()
	defer wallet.RUnlock()
	return wallet.currentHeight
}

// IngestBlock is a call-back which is to be triggered each time a new block is
// connected to the main chain. Ingesting a block updates the wallet's internal
// utxo state based on the outputs created and destroyed within each block.
func (m *InMemoryWallet) IngestBlock(header []byte, filteredTxns [][]byte) {
	var hdr wire.BlockHeader
	if err := hdr.FromBytes(header); err != nil {
		panic(err)
	}
	height := int64(hdr.Height)

	txns := make([]*coinharness.Tx, 0, len(filteredTxns))
	for _, txBytes := range filteredTxns {
		tx, err := m.NewTxFromBytes(txBytes)

		if err != nil {
			panic(err)
		}
		txns = append(txns, tx)
	}

	// Append this new chain update to the end of the queue of new chain
	// updates.
	m.chainMtx.Lock()
	m.chainUpdates = append(m.chainUpdates, &chainUpdate{height, txns})
	m.chainMtx.Unlock()

	// Launch a goroutine to signal the chainSyncer that a new update is
	// available. We do this in a new goroutine in order to avoid blocking
	// the main loop of the rpc client.
	go func() {
		m.chainUpdateSignal <- chainUpdateSignal
	}()
}

//// ingestBlock updates the wallet's internal utxo state based on the outputs
//// created and destroyed within each block.
//func (wallet *InMemoryWallet) ingestBlock(update *chainUpdate) {
//	// Update the latest synced height, then process each filtered
//	// transaction in the block creating and destroying utxos within
//	// the wallet as a result.
//	wallet.currentHeight = update.blockHeight
//	undo := &undoEntry{
//		utxosDestroyed: make(map[coinharness.OutPoint]*utxo),
//	}
//	for _, tx := range update.filteredTxns {
//		mtx := tx.MsgTx()
//		isCoinbase := blockchain.IsCoinBaseTx(mtx)
//		txHash := mtx.TxHash()
//		wallet.evalOutputs(mtx.TxOut, &txHash, isCoinbase, undo)
//		wallet.evalInputs(mtx.TxIn, undo)
//	}
//
//	// Finally, record the undo entry for this block so we can
//	// properly update our internal state in response to the block
//	// being re-org'd from the main chain.
//	wallet.reorgJournal[update.blockHeight] = undo
//}

// chainSyncer is a goroutine dedicated to processing new blocks in order to
// keep the wallet's utxo state up to date.
//
// NOTE: This MUST be run as a goroutine.
func (wallet *InMemoryWallet) chainSyncer() {
	var update *chainUpdate

	for s := range wallet.chainUpdateSignal {
		if s == stopSignal {
			break
		}
		// A new update is available, so pop the new chain update from
		// the front of the update queue.
		wallet.chainMtx.Lock()
		update = wallet.chainUpdates[0]
		wallet.chainUpdates[0] = nil // Set to nil to prevent GC leak.
		wallet.chainUpdates = wallet.chainUpdates[1:]
		wallet.chainMtx.Unlock()

		// Update the latest synced height, then process each filtered
		// transaction in the block creating and destroying utxos within
		// the wallet as a result.
		wallet.Lock()
		wallet.currentHeight = update.blockHeight
		undo := &undoEntry{
			utxosDestroyed: make(map[coinharness.OutPoint]*utxo),
		}
		for _, tx := range update.filteredTxns {
			mtx := tx.MsgTx
			isCoinbase := wallet.IsCoinBaseTx(mtx)
			txHash := mtx.TxHash
			wallet.evalOutputs(mtx.TxOut, txHash, isCoinbase, undo)
			wallet.evalInputs(mtx.TxIn, undo)
		}

		// Finally, record the undo entry for this block so we can
		// properly update our internal state in response to the block
		// being re-org'd from the main chain.
		wallet.reorgJournal[update.blockHeight] = undo
		wallet.Unlock()
	}
}

// evalOutputs evaluates each of the passed outputs, creating a new matching
// utxo within the wallet if we're able to spend the output.
func (wallet *InMemoryWallet) evalOutputs(outputs []*coinharness.TxOut, txHash coinharness.Hash, isCoinbase bool, undo *undoEntry) {
	for i, output := range outputs {
		pkScript := output.PkScript

		// Scan all the addresses we currently control to see if the
		// output is paying to us.
		for keyIndex, addr := range wallet.addrs {
			pkHash := addr.ScriptAddress()
			if !bytes.Contains(pkScript, pkHash) {
				continue
			}

			// If this is a coinbase output, then we mark the
			// maturity height at the proper block height in the
			// future.
			var maturityHeight int64
			if isCoinbase {
				maturityHeight = wallet.currentHeight + int64(wallet.net.CoinbaseMaturity())
			}

			op := coinharness.OutPoint{Hash: txHash, Index: uint32(i)}
			wallet.utxos[op] = &utxo{
				value:          output.Amount.Copy(),
				keyIndex:       keyIndex,
				maturityHeight: maturityHeight,
				pkScript:       pkScript,
			}
			undo.utxosCreated = append(undo.utxosCreated, op)
		}
	}
}

// evalInputs scans all the passed inputs, destroying any utxos within the
// wallet which are spent by an input.
func (wallet *InMemoryWallet) evalInputs(inputs []*coinharness.InputTx, undo *undoEntry) {
	for _, txIn := range inputs {
		op := txIn.PreviousOutPoint
		oldUtxo, ok := wallet.utxos[op]
		if !ok {
			continue
		}

		undo.utxosDestroyed[op] = oldUtxo
		delete(wallet.utxos, op)
	}
}

// UnwindBlock is a call-back which is to be executed each time a block is
// disconnected from the main chain. Unwinding a block undoes the effect that a
// particular block had on the wallet's internal utxo state.
func (m *InMemoryWallet) UnwindBlock(header []byte) {
	var hdr wire.BlockHeader
	if err := hdr.FromBytes(header); err != nil {
		panic(err)
	}
	height := int64(hdr.Height)

	m.Lock()
	defer m.Unlock()

	undo := m.reorgJournal[height]

	for _, utxo := range undo.utxosCreated {
		delete(m.utxos, utxo)
	}

	for outPoint, utxo := range undo.utxosDestroyed {
		m.utxos[outPoint] = utxo
	}

	delete(m.reorgJournal, height)
}

// unwindBlock undoes the effect that a particular block had on the wallet's
// internal utxo state.
func (wallet *InMemoryWallet) unwindBlock(update *chainUpdate) {
	undo := wallet.reorgJournal[update.blockHeight]

	for _, utxo := range undo.utxosCreated {
		delete(wallet.utxos, utxo)
	}

	for outPoint, utxo := range undo.utxosDestroyed {
		wallet.utxos[outPoint] = utxo
	}

	delete(wallet.reorgJournal, update.blockHeight)
}

// newAddress returns a new address from the wallet's hd key chain.  It also
// loads the address into the RPC client's transaction filter to ensure any
// transactions that involve it are delivered via the notifications.
func (wallet *InMemoryWallet) newAddress() (dcrutil.Address, error) {
	index := wallet.hdIndex

	childKey, err := wallet.hdRoot.Child(index)
	if err != nil {
		return nil, err
	}
	privKey, err := childKey.ECPrivKey()
	if err != nil {
		return nil, err
	}

	addr, err := keyToAddr(privKey, wallet.net)
	if err != nil {
		return nil, err
	}

	err = wallet.nodeRPC.Internal().(*rpcclient.Client).LoadTxFilter(false, []dcrutil.Address{addr}, nil)
	if err != nil {
		return nil, err
	}

	wallet.addrs[index] = addr

	wallet.hdIndex++

	return addr, nil
}

// NewAddress returns a fresh address spendable by the wallet.
//
// This function is safe for concurrent access.
func (wallet *InMemoryWallet) NewAddress(_ *coinharness.NewAddressArgs) (coinharness.Address, error) {
	wallet.Lock()
	defer wallet.Unlock()

	add, err := wallet.newAddress()

	if err != nil {
		return nil, err
	}

	return &dcrharness.Address{Address: add}, nil
}

//// fundTx attempts to fund a transaction sending amt coins.  The coins are
//// selected such that the final amount spent pays enough fees as dictated by
//// the passed fee rate.  The passed fee rate should be expressed in
//// atoms-per-byte.
////
//// NOTE: The InMemoryWallet's mutex must be held when this function is called.
//func (wallet *InMemoryWallet) fundTx(tx *wire.MsgTx, amt dcrutil.Amount, feeRate dcrutil.Amount) error {
//	const (
//		// spendSize is the largest number of bytes of a sigScript
//		// which spends a p2pkh output: OP_DATA_73 <sig> OP_DATA_33 <pubkey>
//		spendSize = 1 + 73 + 1 + 33
//	)
//
//	var (
//		amtSelected dcrutil.Amount
//		txSize      int
//	)
//

//
//	// If we've reached this point, then coin selection failed due to an
//	// insufficient amount of coins.
//	return fmt.Errorf("not enough funds for coin selection")
//}

// SendOutputs creates, then sends a transaction paying to the specified output
// while observing the passed fee rate. The passed fee rate should be expressed
// in satoshis-per-byte.
func (wallet *InMemoryWallet) SendOutputs(args *coinharness.SendOutputsArgs) (coinharness.Hash, error) {
	arg2 := &coinharness.CreateTransactionArgs{
		Outputs: args.Outputs,
		FeeRate: args.FeeRate,
	}
	tx, err := coinharness.CreateTransaction(wallet, arg2)
	if err != nil {
		return nil, err
	}

	return wallet.nodeRPC.SendRawTransaction(tx, true)
}

// SendOutputsWithoutChange creates and sends a transaction that pays to the
// specified outputs while observing the passed fee rate and ignoring a change
// output. The passed fee rate should be expressed in sat/b.
func (wallet *InMemoryWallet) SendOutputsWithoutChange(outputs []*wire.TxOut,
	feeRate dcrutil.Amount) (*chainhash.Hash, error) {

	//cast list
	b := make([]coinharness.TxOut, len(outputs))
	{
		for i := range outputs {
			b[i] = &dcrharness.OutputTx{outputs[i]}
		}
	}
	args := &coinharness.CreateTransactionArgs{
		Outputs: b,
		FeeRate: feeRate,
		Change:  false,
	}
	tx, err := coinharness.CreateTransaction(wallet, args)
	if err != nil {
		return nil, err
	}

	r, x := wallet.nodeRPC.SendRawTransaction(tx, true)
	return r.(*chainhash.Hash), x
}

//// CreateTransaction returns a fully signed transaction paying to the specified
//// outputs while observing the desired fee rate. The passed fee rate should be
//// expressed in satoshis-per-byte. The transaction being created can optionally
//// include a change output indicated by the change boolean.
////
//// This function is safe for concurrent access.
//func (wallet *InMemoryWallet) CreateTransaction(args *coinharness.CreateTransactionArgs) (coinharness.MessageTx, error) {
//

//}

func (wallet *InMemoryWallet) ListUnspent() (result []*coinharness.Unspent, err error) {
	wallet.Lock()
	defer wallet.Unlock()

	for _, utxo := range wallet.utxos {
		// Skip any outputs that are still currently immature or are
		// currently locked.
		if !utxo.isMature(wallet.currentHeight) || utxo.isLocked {
			continue
		}
		x := &coinharness.Unspent{
			Account:      "",
			Amount:       utxo.value,
		}
		result = append(result, x)

		// Calculate the fee required for the txn at this point
		// observing the specified fee rate. If we don't have enough
		// coins from he current amount selected to pay the fee, then
		// continue to grab more coins.
		reqFee := dcrutil.Amount(txSize * int(feeRate))
		if amtSelected-reqFee < amt {
			continue
		}

		// If we have any change left over, then add an additional
		// output to the transaction reserved for change.
		changeVal := amtSelected - amt - reqFee
		if changeVal > 0 {
			addr, err := wallet.newAddress()
			if err != nil {
				return err
			}
			pkScript, err := txscript.PayToAddrScript(addr)
			if err != nil {
				return err
			}
			changeOutput := &wire.TxOut{
				Value:    int64(changeVal),
				PkScript: pkScript,
			}
			tx.AddTxOut(changeOutput)
		}

		return nil
	}

	if err := wallet.fundTx(tx, outputAmt, dcrutil.Amount(args.FeeRate.(int))); err != nil {
		return nil, err
	}

	// Populate all the selected inputs with valid sigScript for spending.
	// Along the way record all outputs being spent in order to avoid a
	// potential double spend.
	spentOutputs := make([]*utxo, 0, len(tx.TxIn))
	for i, txIn := range tx.TxIn {
		outPoint := txIn.PreviousOutPoint
		utxo := wallet.utxos[outPoint]

		extendedKey, err := wallet.hdRoot.Child(utxo.keyIndex)
		if err != nil {
			return nil, err
		}

		privKey, err := extendedKey.ECPrivKey()
		if err != nil {
			return nil, err
		}

		sigScript, err := txscript.SignatureScript(tx, i, utxo.pkScript,
			txscript.SigHashAll, privKey, true)
		if err != nil {
			return nil, err
		}

		txIn.SignatureScript = sigScript

		spentOutputs = append(spentOutputs, utxo)
	}

	// As these outputs are now being spent by this newly created
	// transaction, mark the outputs are "locked". This action ensures
	// these outputs won't be double spent by any subsequent transactions.
	// These locked outputs can be freed via a call to UnlockOutputs.
	for _, utxo := range spentOutputs {
		utxo.isLocked = true
	}
	return &dcrharness.MessageTx{tx}, nil
}

// UnlockOutputs unlocks any outputs which were previously locked due to
// being selected to fund a transaction via the CreateTransaction method.
//
// This function is safe for concurrent access.
func (wallet *InMemoryWallet) UnlockOutputs(inputs []coinharness.InputTx) error {
	wallet.Lock()
	defer wallet.Unlock()

	for _, input := range inputs {
		utxo, ok := wallet.utxos[input.PreviousOutPoint]
		if !ok {
			continue
		}

		utxo.isLocked = false
	}

	return nil
}

// GetBalance returns the confirmed balance of the wallet.
//
// This function is safe for concurrent access.
func (wallet *InMemoryWallet) GetBalance(account string) (*coinharness.GetBalanceResult, error) {
	wallet.RLock()
	defer wallet.RUnlock()
	result := &coinharness.GetBalanceResult{}
	balance := coinharness.CoinsAmount{0}
	for _, utxo := range wallet.utxos {
		// Prevent any immature or locked outputs from contributing to
		// the wallet's total confirmed balance.
		if !utxo.isMature(wallet.currentHeight) || utxo.isLocked {
			continue
		}

		balance.AtomsValue += utxo.value.AtomsValue
	}

	result.TotalSpendable = balance
	return result, nil
}

func (wallet *InMemoryWallet) RPCClient() *coinharness.RPCConnection {
	panic("Method not supported")
}

func (wallet *InMemoryWallet) CreateNewAccount(accountName string) error {
	panic("")
}

func (wallet *InMemoryWallet) GetNewAddress(accountName string) (coinharness.Address, error) {
	panic("")
}
func (wallet *InMemoryWallet) ValidateAddress(address coinharness.Address) (*coinharness.ValidateAddressResult, error) {
	panic("")
}

func (wallet *InMemoryWallet) WalletUnlock(password string, seconds int64) error {
	return nil
}
func (wallet *InMemoryWallet) WalletInfo() (*coinharness.WalletInfoResult, error) {
	return &coinharness.WalletInfoResult{
		Unlocked: true,
	}, nil
}
func (wallet *InMemoryWallet) WalletLock() error {
	return nil
}
