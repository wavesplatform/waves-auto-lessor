package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/oguzbilgic/fpd"
	"github.com/wavesplatform/gowaves/pkg/client"
	"github.com/wavesplatform/gowaves/pkg/crypto"
	"github.com/wavesplatform/gowaves/pkg/proto"
)

const (
	waves                = 100000000
	defaultScheme        = "http"
	standardFee   uint64 = 100000
)

var (
	version              = "v0.0.0"
	errInvalidParameters = errors.New("invalid parameters")
	errUserTermination   = errors.New("user termination")
	errFailure           = errors.New("operation failure")
	na                   = proto.OptionalAsset{}
)

type feature struct {
	ID               int    `json:"id"`
	Description      string `json:"description"`
	BlockchainStatus string `json:"blockchainStatus"`
	NodeStatus       string `json:"nodeStatus"`
	ActivationHeight int    `json:"activationHeight"`
}

type activationStatusResponse struct {
	Height          int       `json:"height"`
	VotingInterval  int       `json:"votingInterval"`
	VotingThreshold int       `json:"votingThreshold"`
	NextCheck       int       `json:"nextCheck"`
	Features        []feature `json:"features"`
}

type account struct {
	sk   crypto.SecretKey
	pk   crypto.PublicKey
	addr proto.WavesAddress
}

func (a *account) recipient() proto.Recipient {
	return proto.NewRecipientFromAddress(a.addr)
}

func (a *account) String() string {
	return a.addr.String()
}

func accountFromSK(sk crypto.SecretKey, scheme byte) (account, error) {
	pk := crypto.GeneratePublicKey(sk)
	a, err := proto.NewAddressFromPublicKey(scheme, pk)
	if err != nil {
		return account{}, err
	}
	return account{
		sk:   sk,
		pk:   pk,
		addr: a,
	}, nil
}

func accountFromSKAndDifferentPK(sk crypto.SecretKey, pk crypto.PublicKey, scheme byte) (account, error) {
	a, err := proto.NewAddressFromPublicKey(scheme, pk)
	if err != nil {
		return account{}, err
	}
	return account{
		sk:   sk,
		pk:   pk,
		addr: a,
	}, nil
}

func accountFromAddress(addr proto.WavesAddress) account {
	return account{addr: addr}
}

func main() {
	err := run()
	if err != nil {
		switch err {
		case errInvalidParameters:
			showUsage()
			os.Exit(2)
		case errUserTermination:
			os.Exit(130)
		case errFailure:
			os.Exit(70)
		default:
			os.Exit(1)
		}
	}
}

func run() error {
	var (
		nodeURL            string
		generatorSK        string
		lessorSK           string
		lessorPK           string
		leasingAddress     string
		irreducibleBalance int64
		leasingThreshold   int64
		transferOnly       bool
		recipientAddress   string
		dryRun             bool
		testRun            bool
		showHelp           bool
		showVersion        bool
	)
	flag.StringVar(&nodeURL, "node-api", "http://localhost:6869", "Node's REST API URL")
	flag.StringVar(&generatorSK, "generating-sk", "", "Base58 encoded private key of generating account")
	flag.StringVar(&lessorSK, "lessor-sk", "", "Base58 encoded private key of lessor")
	flag.StringVar(&lessorPK, "lessor-pk", "", "Base58 encoded lessor's public key")
	flag.BoolVar(&transferOnly, "transfer-only", false, "Do not create leasing transaction")
	flag.StringVar(&recipientAddress, "recipient-address", "", "Base58 encoded recipient address, used in 'transfer only' mode")
	flag.StringVar(&leasingAddress, "leasing-address", "", "Base58 encoded leasing address if differs from generating account")
	flag.Int64Var(&irreducibleBalance, "irreducible-balance", waves, "Irreducible balance on accounts in WAVELETS, default value is 1 Waves")
	flag.Int64Var(&leasingThreshold, "leasing-threshold", 0, "Leasing amount threshold in WAVELETS, a leasing transaction created only if amount is bigger than the given value")
	flag.BoolVar(&dryRun, "dry-run", false, "Test execution without creating real transactions on blockchain")
	flag.BoolVar(&testRun, "test-run", false, "Test execution with limited available balance of 1 WAVES")
	flag.BoolVar(&showHelp, "help", false, "Show usage information and exit")
	flag.BoolVar(&showVersion, "version", false, "Print version information and quit")
	flag.Parse()

	if showHelp {
		showUsage()
		return nil
	}
	if showVersion {
		fmt.Printf("Waves Automatic Lessor %s\n", version)
		return nil
	}

	if nodeURL == "" {
		log.Println("[ERROR] Empty node's URL. Please, provide correct URL to node.")
		return errInvalidParameters
	}
	u, err := normalizeURL(nodeURL)
	if err != nil {
		log.Printf("[ERROR] Invalid node's URL '%s': %v", nodeURL, err)
	}
	nodeURL = u

	if generatorSK == "" {
		log.Println("[ERROR] Empty generating account private key. Please, provide the correct private key.")
		return errInvalidParameters
	}
	gSK, err := crypto.NewSecretKeyFromBase58(generatorSK)
	if err != nil {
		log.Printf("[ERROR] Invalid generating account private key '%s': %v", generatorSK, err)
		return errInvalidParameters
	}
	var (
		lSK                      crypto.SecretKey
		differentLessorPK        *crypto.PublicKey
		leasingAddr              *proto.WavesAddress
		transferRecipientAddress proto.WavesAddress
	)
	if transferOnly {
		log.Println("[INFO] Transfer only mode activated")
		if recipientAddress == "" {
			log.Println("[ERROR] Empty recipient address. Please, provide the correct recipient address.")
			return errInvalidParameters
		}
		a, err := proto.NewAddressFromString(recipientAddress)
		if err != nil {
			log.Printf("[ERROR] Invalid transfer recipient address '%s': %v", recipientAddress, err)
			return errInvalidParameters
		}
		transferRecipientAddress = a
	} else {
		if lessorSK == "" {
			log.Println("[ERROR] Empty lessor private key. Please, provide correct lessor private key.")
			return errInvalidParameters
		}
		var err error
		lSK, err = crypto.NewSecretKeyFromBase58(lessorSK)
		if err != nil {
			log.Printf("[ERROR] Invalid lessor private key '%s': %v", lessorSK, err)
			return errInvalidParameters
		}
		if lessorPK == "" {
			log.Print("[INFO] No different lessor public key is given")
		} else {
			pk, err := crypto.NewPublicKeyFromBase58(lessorPK)
			if err != nil {
				log.Printf("[ERROR] Failed to parse additional lessor public key'%s': %v", lessorPK, err)
				return errFailure
			}
			differentLessorPK = &pk
		}
		if leasingAddress == "" {
			log.Printf("[INFO] No different leasing address is given")
		} else {
			a, err := proto.NewAddressFromString(leasingAddress)
			if err != nil {
				log.Printf("[ERROR] Invalid leasing address '%s': %v", leasingAddress, err)
				return errFailure
			}
			leasingAddr = &a
		}
	}
	if irreducibleBalance < 0 {
		log.Printf("[ERROR] Invalid irreducible balance value '%d'", irreducibleBalance)
		return errInvalidParameters
	}
	if irreducibleBalance > 0 {
		log.Printf("[INFO] Accounts irreducible balance set to %s", format(uint64(irreducibleBalance)))
	}
	if testRun {
		log.Printf("[INFO] TEST-RUN: Available balance will be limited to %s", format(waves))
	}
	if dryRun {
		log.Print("[INFO] DRY-RUN: No actual transactions will be created")
	}

	ctx, done := signal.NotifyContext(context.Background(), os.Interrupt)
	defer done()

	// 1. Check connection to node's API
	cl, err := nodeClient(ctx, nodeURL)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to connect to node at '%s': %v", nodeURL, err)
		return errFailure
	}
	log.Printf("[INFO] Successfully connected to '%s'", cl.GetOptions().BaseUrl)

	// 2. Acquire the network scheme from genesis block and Protobuf activation status
	scheme, err := getScheme(ctx, cl)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to aquire blockchain scheme: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Blockchain scheme: %s", string(scheme))
	protobuf, err := isProtobufActivated(ctx, cl)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to check Protobuf activation status: %v", err)
		return errFailure
	}
	var txVer byte = 2
	if protobuf {
		txVer = 3
	}
	log.Printf("[INFO] Version of transactions to produce: %d", txVer)

	// 3. Generate public keys and addresses from given private keys
	generator, err := accountFromSK(gSK, scheme)
	if err != nil {
		log.Printf("[ERROR] Failed to create generator's account: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Generating address: %s", generator.String())
	var (
		transferRecipient account
		lessor            account
		leasingRecipient  account
	)
	if transferOnly {
		transferRecipient = accountFromAddress(transferRecipientAddress)
		log.Printf("[INFO] Transfer recipient address: %s", transferRecipient.String())
	} else {
		if differentLessorPK != nil {
			lessor, err = accountFromSKAndDifferentPK(lSK, *differentLessorPK, scheme)
			if err != nil {
				log.Printf("[ERROR] Failed to create lessor account: %v", err)
				return errFailure
			}
		} else {
			lessor, err = accountFromSK(lSK, scheme)
			if err != nil {
				log.Printf("[ERROR] Failed to create lessor account: %v", err)
				return errFailure
			}
		}
		transferRecipient = lessor

		leasingRecipient = generator
		if leasingAddr != nil { // If different leasing address was provided make recipient of it
			leasingRecipient = accountFromAddress(*leasingAddr)
		}
		log.Printf("[INFO] Lessor address: %s", lessor.String())
		log.Printf("[INFO] Lessor public key: %s", lessor.pk.String())
		log.Printf("[INFO] Leasing to address: %s", leasingRecipient.String())
	}

	// 4. Check available WAVES balance on generating address
	balance, err := getAvailableWavesBalance(ctx, cl, generator.addr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to get generator WAVES balance: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Balance of generation account '%s': %s", generator.String(), format(balance))
	if irreducibleBalance > 0 {
		b := int64(balance) - irreducibleBalance
		if b > 0 {
			balance = uint64(b)
		} else {
			balance = 0
		}
	}
	if balance <= standardFee {
		log.Print("[ERROR] Not enough balance on generator's account")
		return errFailure
	}
	if balance > waves && testRun {
		balance = waves
	}
	log.Printf("[INFO] Balance available for transfer: %s", format(balance))

	// 5. Create transfer transaction to lessor account
	transferExtraFee, err := getExtraFee(ctx, cl, generator.addr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to check extra fee on account '%s': %v", generator.String(), err)
		return errFailure
	}
	if transferExtraFee != 0 {
		log.Printf("[INFO] Extra fee on transfer: %s", format(transferExtraFee))
	} else {
		log.Print("[INFO] No extra fee on transfer")
	}
	fee := standardFee + transferExtraFee
	amount := balance - fee
	if amount <= 0 {
		log.Print("[ERROR] Negative of zero amount to transfer")
		return errFailure
	}
	transfer := proto.NewUnsignedTransferWithProofs(txVer, generator.pk, na, na, timestamp(), amount, fee, transferRecipient.recipient(), nil)
	err = transfer.Sign(scheme, generator.sk)
	if err != nil {
		log.Printf("[ERROR] Failed to sign transfer transaction: %v", err)
		return errFailure
	}
	if dryRun {
		b, err := json.Marshal(transfer)
		if err != nil {
			log.Printf("[ERROR] Failed to make transaction json: %v", err)
			return errFailure
		}
		log.Printf("[INFO] Transfer transaction:\n%s", string(b))
	} else {
		log.Printf("[INFO] Transfer transaction ID: %s", transfer.ID.String())
		err = broadcast(ctx, cl, transfer)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to broadcast transfer transaction: %v", err)
			return errFailure
		}
		err = track(ctx, cl, *transfer.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to track transfer transaction: %v", err)
			return errFailure
		}
	}
	if transferOnly { // Early exit in transfer only mode
		log.Print("[INFO] OK")
		return nil
	}

	// 6. Check WAVES balance on lessor's account
	balance, err = getAvailableWavesBalance(ctx, cl, lessor.addr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to get lessor account's WAVES balance: %v", err)
		return errFailure
	}
	log.Printf("[INFO] Balance of lessor account '%s': %s", lessor.String(), format(balance))
	if irreducibleBalance > 0 {
		b := int64(balance) - irreducibleBalance
		if b > 0 {
			balance = uint64(b)
		} else {
			balance = 0
		}
	}
	if balance <= standardFee {
		log.Print("[ERROR] Not enough balance on lessor's account")
		return errFailure
	}
	if balance > waves && testRun {
		balance = waves
	}
	log.Printf("[INFO] Balance available for leasing: %s", format(balance))

	// 7. Create leasing transaction
	leaseExtraFee, err := getExtraFee(ctx, cl, lessor.addr)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errUserTermination
		}
		log.Printf("[ERROR] Failed to check extra fee on account '%s': %v", lessor.String(), err)
		return errFailure
	}
	if leaseExtraFee != 0 {
		log.Printf("[INFO] Extra fee on lease: %s", format(leaseExtraFee))
	} else {
		log.Print("[INFO] No extra fee on lease")
	}
	fee = standardFee + leaseExtraFee
	amount = balance - fee
	if amount <= 0 {
		log.Print("[ERROR] Negative of zero amount to lease")
		return errFailure
	}
	if leasingThreshold > 0 {
		if amount < uint64(leasingThreshold) {
			log.Printf("[INFO] Leasing amount %d is less than threshold %d", amount, leasingThreshold)
			return nil
		}
	}
	lease := proto.NewUnsignedLeaseWithProofs(txVer, lessor.pk, leasingRecipient.recipient(), amount, fee, timestamp())
	err = lease.Sign(scheme, lSK)
	if err != nil {
		log.Printf("[ERROR] Failed to sign lease transaction: %v", err)
		return errFailure
	}
	if dryRun {
		b, err := json.Marshal(lease)
		if err != nil {
			log.Printf("[ERROR] Failed to make transaction json: %v", err)
			return errFailure
		}
		log.Printf("[INFO] Lease transaction:\n%s", string(b))
	} else {
		log.Printf("[INFO] Lease transaction ID: %s", lease.ID.String())
		err = broadcast(ctx, cl, lease)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to broadcast lease transaction: %v", err)
			return errFailure
		}
		err = track(ctx, cl, *lease.ID)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return errUserTermination
			}
			log.Printf("[ERROR] Failed to track lease transaction: %v", err)
			return errFailure
		}
	}
	log.Print("[INFO] OK")
	return nil
}

func broadcast(ctx context.Context, cl *client.Client, tx proto.Transaction) error {
	_, err := cl.Transactions.Broadcast(ctx, tx)
	return err
}

func track(ctx context.Context, cl *client.Client, id crypto.Digest) error {
	log.Printf("[INFO] Waiting for transaction '%s' on blockchain...", id.String())
	for {
		_, rsp, err := cl.Transactions.Info(ctx, id)
		if errors.Is(err, context.Canceled) {
			return err
		}
		if rsp.StatusCode == http.StatusOK {
			return nil
		}
		time.Sleep(time.Second)
	}
}

func timestamp() uint64 {
	return uint64(time.Now().UnixNano()) / 1000000
}

func format(amount uint64) string {
	da := fpd.New(int64(amount), -8)
	return fmt.Sprintf("%s WAVES", da.FormattedString())
}

func getAvailableWavesBalance(ctx context.Context, cl *client.Client, addr proto.WavesAddress) (uint64, error) {
	ab, _, err := cl.Addresses.BalanceDetails(ctx, addr)
	if err != nil {
		return 0, err
	}
	return ab.Available, nil
}

func getExtraFee(ctx context.Context, cl *client.Client, addr proto.WavesAddress) (uint64, error) {
	info, _, err := cl.Addresses.ScriptInfo(ctx, addr)
	if err != nil {
		return 0, err
	}
	return info.ExtraFee, nil
}

func normalizeURL(s string) (string, error) {
	if !strings.Contains(s, "//") {
		s = "//" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("unsupported URL scheme '%s'", u.Scheme)
	}
	return u.String(), nil
}

func nodeClient(ctx context.Context, s string) (*client.Client, error) {
	var u *url.URL
	var err error
	if strings.Contains(s, "//") {
		u, err = url.Parse(s)
	} else {
		u, err = url.Parse("//" + s)
	}
	if err != nil {
		return nil, err
	}
	if u.Scheme == "" {
		u.Scheme = defaultScheme
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("unsupported URL scheme '%s'", u.Scheme)
	}
	cl, err := client.NewClient(client.Options{BaseUrl: u.String(), Client: &http.Client{}})
	if err != nil {
		return nil, err
	}
	_, _, err = cl.Blocks.Height(ctx)
	if err != nil {
		return nil, err
	}
	return cl, nil
}

func getScheme(ctx context.Context, cl *client.Client) (proto.Scheme, error) {
	b, _, err := cl.Blocks.Last(ctx)
	if err != nil {
		return 0, err
	}
	return b.Generator.Bytes()[1], nil
}

func isProtobufActivated(ctx context.Context, cl *client.Client) (bool, error) {
	statusRequest, err := http.NewRequest("GET", cl.GetOptions().BaseUrl+"/activation/status", nil)
	if err != nil {
		return false, err
	}
	resp := new(activationStatusResponse)
	_, err = cl.Do(ctx, statusRequest, resp)
	if err != nil {
		return false, err
	}
	for _, f := range resp.Features {
		if f.ID == 15 && f.BlockchainStatus == "ACTIVATED" && (f.NodeStatus == "IMPLEMENTED" || f.NodeStatus == "VOTED") {
			return true, nil
		}
	}
	return false, nil
}

func showUsage() {
	_, _ = fmt.Fprintf(os.Stderr, "\nUsage of Waves Automatic Lessor %s\n", version)
	flag.PrintDefaults()
}
