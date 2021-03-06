package client

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/raft-kv-store/common"
	"github.com/raft-kv-store/raftpb"
)

var (
	CmdRegex = regexp.MustCompile(`[^\s"']+|"([^"]*)"|'([^']*)\n`)
)

const maxTransferRetries = 5

type insufficientFundsError struct {
	key    string
	amount int64
}

func (err *insufficientFundsError) Error() string {
	return fmt.Sprintf("insufficient funds: %d in %s", err.amount, err.key)
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func addURLScheme(s string) string {
	if strings.HasPrefix(s, "https://") {
		s = strings.Replace(s, "https://", "http://", 1)
		return s
	} else if !strings.HasPrefix(s, "http://") {
		return "http://" + s
	}
	return s
}

type raftKVClient struct {
	client     *http.Client
	serverAddr string
	// TODO: Add stop API to avoid exposing Terminate channel
	Terminate chan os.Signal
	reader    *bufio.Reader
	inTxn     bool
	txnCmds   *raftpb.RaftCommand
}

func NewRaftKVClient(serverAddr string) *raftKVClient {
	c := &raftKVClient{
		client:     &http.Client{Timeout: 5 * time.Second},
		serverAddr: addURLScheme(serverAddr),
		Terminate:  make(chan os.Signal, 1),
		reader:     bufio.NewReader(os.Stdin),
		txnCmds:    &raftpb.RaftCommand{},
	}
	return c
}

func (c *raftKVClient) setServerAddr(newAddr string) {
	c.serverAddr = addURLScheme(newAddr)
}

func (c *raftKVClient) readString() []string {
	var cmdArr []string
	fmt.Print(">")
	cmdStr, err := c.reader.ReadString('\n')
	if err != nil {
		log.Fatal(err)
	}
	cmdStr = strings.TrimSuffix(cmdStr, "\n")
	// To gather quotes
	cmdArr = CmdRegex.FindAllString(cmdStr, -1)
	for i := range cmdArr {
		cmdArr[i] = strings.Trim(cmdArr[i], "'\"")
	}
	return cmdArr
}

func (c *raftKVClient) validCmd2(cmdArr []string) error {
	if len(cmdArr) != 2 {
		return fmt.Errorf("Invalid %[1]s command. Correct syntax: %[1]s [key]", cmdArr[0])
	}
	return nil
}

func (c *raftKVClient) validCmd3(cmdArr []string) error {
	if len(cmdArr) != 3 {
		return fmt.Errorf("Invalid %[1]s command. Correct syntax: %[1]s [key] [value]", cmdArr[0])
	}
	if _, ok := parseInt64(cmdArr[2]); ok != nil {
		return fmt.Errorf("Invalid %s command. Error in parsing %s as numerical value", cmdArr[0], cmdArr[2])
	}
	return nil
}

func (c *raftKVClient) validTxn(cmdArr []string) error {
	if c.inTxn {
		return errors.New("Already in transaction")
	}
	if len(cmdArr) != 1 {
		return errors.New("Invalid transaction command. Correct syntax: txn")
	}
	return nil
}

func (c *raftKVClient) validEndTxn(cmdArr []string) error {
	if !c.inTxn {
		return errors.New("Not in transaction")
	}
	if len(cmdArr) != 1 {
		return errors.New("Invalid end transaction command. Correct syntax: end")
	}
	return nil
}

func (c *raftKVClient) validExit(cmdArr []string) error {
	if len(cmdArr) != 1 {
		return errors.New("Invalid exit command. Correct syntax: exit")
	}
	return nil
}

// Simpler version of 'transfer' command, eg., Issuing `transfer x y 10` is translated
// to `transfer 10 from x to y`
func (c *raftKVClient) validTxnTransfer(cmdArr []string) error {
	if len(cmdArr) != 4 {
		return fmt.Errorf("invalid %[1]s command. Correct syntax: %[1]s [fromKey] [toKey] "+
			"[amount to be transferred]", cmdArr[0])
	}

	if _, ok := parseInt64(cmdArr[3]); ok != nil {
		return fmt.Errorf("invalid %s command. Error in parsing %s as numerical value", cmdArr[0], cmdArr[3])
	}

	return nil
}

func (c *raftKVClient) validCmd(cmdArr []string) error {
	if len(cmdArr) == 0 {
		return errors.New("")
	}
	switch cmdArr[0] {
	case common.GET, common.DEL:
		return c.validCmd2(cmdArr)
	case common.SET, common.ADD, common.SUB:
		return c.validCmd3(cmdArr)
	case common.TXN:
		return c.validTxn(cmdArr)
	case common.ENDTXN:
		return c.validEndTxn(cmdArr)
	case common.EXIT:
		return c.validExit(cmdArr)
	case common.TRANSFER:
		return c.validTxnTransfer(cmdArr)
	default:
		return errors.New("Command not recognized.")
	}
}

func (c *raftKVClient) TransactionRun(cmdArr []string) {
	switch cmdArr[0] {
	case common.TXN:
		c.inTxn = true
		c.txnCmds = &raftpb.RaftCommand{}
		fmt.Println("Entering transaction status")
	case common.SET:
		val, _ := parseInt64(cmdArr[2])
		c.txnCmds.Commands = append(c.txnCmds.Commands, &raftpb.Command{
			Method: common.SET,
			Key:    cmdArr[1],
			Value:  val,
		})
	case common.DEL:
		c.txnCmds.Commands = append(c.txnCmds.Commands, &raftpb.Command{
			Method: common.DEL,
			Key:    cmdArr[1],
		})
	case common.ADD, common.SUB:
		fmt.Println("Not implemented")
	case common.ENDTXN:
		if _, err := c.Transaction(); err != nil {
			fmt.Println(err)
		}
		c.inTxn = false
	case common.EXIT:
		fmt.Println("Stop client")
		os.Exit(0)
	default:
		fmt.Println("Only set and delete command are available in transaction.")
	}
}

func (c *raftKVClient) TransferTransaction(cmdArr []string) error {
	fromKey := cmdArr[1]
	toKey := cmdArr[2]
	transferAmount, _ := parseInt64(cmdArr[3])
	var err error

	if transferAmount == 0 {
		return fmt.Errorf("invalid transfer amount %d, so aborting the txn", transferAmount)
	}

	retries := 0
	for retries < maxTransferRetries {
		err = c.attemptTransfer(fromKey, toKey, transferAmount)
		if err != nil {
			retries++
			var e *insufficientFundsError
			if errors.As(err, &e) {
				return fmt.Errorf("%s, so aborting the txn", err)
			}
			fmt.Printf("%s\n Retrying...\n", err)
		} else {
			fmt.Printf("xfer succeeded from %s to %s\n", fromKey, toKey)
			return nil
		}
	}

	return fmt.Errorf("%s\n retries exhausted, aborting txn", err)
}

func (c *raftKVClient) attemptTransfer(fromKey, toKey string, transferAmount int64) error {

	var fromValue int64
	var toValue int64

	/* Order of transactions to be sent to raft server. eg., transfer x y 5
	* 1. Fetch values for the fromKey, toKey in a single transaction.
	* 2. If successful, then send another transaction with the updated values for those keys.
		  set x server_fetched_value - 5
	      set y server_fetched_value + 5
	* 3. If successful, return back to the client with a success, fail for all other cases.
	*/
	c.txnCmds = &raftpb.RaftCommand{
		Commands: []*raftpb.Command{
			{Method: common.GET, Key: fromKey},
			{Method: common.GET, Key: toKey},
		},
	}

	// Send txn to the server & fetch the response
	getTxnRsp, err := c.Transaction()
	if err != nil {
		return fmt.Errorf("get txn failed with err: %s", err.Error())
	}

	for _, cmdRsp := range getTxnRsp.Commands {
		if cmdRsp.Key == fromKey {
			fromValue = cmdRsp.Value
		} else if cmdRsp.Key == toKey {
			toValue = cmdRsp.Value
		}
	}

	if fromValue < transferAmount {
		return fmt.Errorf("%w", &insufficientFundsError{key: fromKey, amount: transferAmount})
	}

	c.txnCmds = &raftpb.RaftCommand{
		Commands: []*raftpb.Command{
			{Method: common.SET, Key: fromKey, Value: fromValue - transferAmount, // new value
				Cond: &raftpb.Cond{
					Key:   fromKey,
					Value: fromValue, // old value
				},
			},
			{Method: common.SET, Key: toKey, Value: toValue + transferAmount, // new value
				Cond: &raftpb.Cond{
					Key:   toKey,
					Value: toValue, // old value
				},
			},
		},
	}

	_, err = c.Transaction()
	if err != nil {
		return fmt.Errorf("set txn failed with err: %s", err.Error())
	}

	return nil
}

func (c *raftKVClient) Run() {
	for {
		cmdArr := c.readString()
		if err := c.validCmd(cmdArr); err != nil {
			if err.Error() != "" {
				fmt.Println(err)
			}
			continue
		}
		if c.inTxn {
			c.TransactionRun(cmdArr)
			continue
		}
		switch cmdArr[0] {
		case common.GET:
			if err := c.Get(cmdArr[1]); err != nil {
				fmt.Println(err)
			}
		case common.SET:
			val, _ := parseInt64(cmdArr[2])
			if err := c.Set(cmdArr[1], val); err != nil {
				fmt.Println(err)
			}
		case common.DEL:
			if err := c.Delete(cmdArr[1]); err != nil {
				fmt.Println(err)
			}
		case common.ADD, common.SUB:
			fmt.Println("Not implemented")
		case common.TXN:
			c.TransactionRun(cmdArr)
		case common.TRANSFER:
			if err := c.TransferTransaction(cmdArr); err != nil {
				fmt.Println(err)
			}
		case common.EXIT:
			fmt.Println("Stop client")
			os.Exit(0)
		}
	}
}

func (c *raftKVClient) parseServerAddr(key string) (string, error) {
	u, err := url.Parse(c.serverAddr)
	if err != nil {
		return "", err
	}
	u.Path = path.Join(u.Path, "key", key)
	return u.String(), nil
}

func (c *raftKVClient) newRequest(method, key string, data []byte) (*http.Response, error) {
	url, err := c.parseServerAddr(key)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(method, url, bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *raftKVClient) newTxnRequest(data []byte) (*http.Response, error) {
	u, err := url.Parse(c.serverAddr)
	if err != nil {
		return nil, err
	}
	u.Path = path.Join(u.Path, "transaction")
	req, err := http.NewRequest(http.MethodPost, u.String(), bytes.NewBuffer(data))
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *raftKVClient) Get(key string) error {
	resp, err := c.newRequest(http.MethodGet, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		fmt.Println(string(body))
		return nil
	}
	return errors.New(string(body))
}

func (c *raftKVClient) Set(key string, value int64) error {
	var reqBody []byte
	var err error
	if reqBody, err = proto.Marshal(&raftpb.Command{
		Method: common.SET,
		Key:    key,
		Value:  value,
	}); err != nil {
		return err
	}
	resp, err := c.newRequest(http.MethodPost, key, reqBody)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		fmt.Println("OK")
		return nil
	}
	return errors.New(string(body))
}

func (c *raftKVClient) Delete(key string) error {
	resp, err := c.newRequest(http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusOK {
		fmt.Println("OK")
		return nil
	}
	return errors.New(string(body))
}

func (c *raftKVClient) OptimizeTxnCommands() {
	lastSetMap := make(map[string]int)
	txnSkips := make([]bool, len(c.txnCmds.Commands))
	/* lastSetMap contains only valid keys (no `del` cmd followed by `set` in input cmd seq).
	*  If Keys exist, they are mapped to the index of last "set cmd" in the input cmd seq.
	*
	*  Also, to guarantee same ordering of commands as the input in txn, maintain separate
	* array txnSkips to skip values containing `True`.
	 */
	for idx, cmd := range c.txnCmds.Commands {
		switch cmd.Method {
		case common.SET:
			val, ok := lastSetMap[cmd.Key]
			if ok {
				txnSkips[val] = true // skip
			}
			lastSetMap[cmd.Key] = idx
		case common.DEL:
			val, ok := lastSetMap[cmd.Key]
			if ok {
				txnSkips[val] = true // skip
				txnSkips[idx] = true // skip
				delete(lastSetMap, cmd.Key)
			}
		}
	}

	var newCmds []*raftpb.Command
	for idx, ifSkip := range txnSkips {
		if !ifSkip {
			newCmds = append(newCmds, c.txnCmds.Commands[idx])
		}
	}
	c.txnCmds.Commands = newCmds
}

func (c *raftKVClient) Transaction() (*raftpb.RaftCommand, error) {
	oldLen := len(c.txnCmds.Commands)
	c.OptimizeTxnCommands()
	newLen := len(c.txnCmds.Commands)
	if newLen == 0 {
		fmt.Println("txn takes no effect so not submitting to server")
		return nil, nil
	} else if newLen < oldLen {
		fmt.Printf("Optimized txn to %v: \n", c.txnCmds.Commands)
	}

	if newLen == 1 {
		return nil, c.txnToSingleCmd()
	}

	fmt.Printf("Submitting %v\n", c.txnCmds.Commands)
	var reqBody []byte
	var err error
	if reqBody, err = proto.Marshal(c.txnCmds); err != nil {
		return nil, err
	}
	resp, err := c.newTxnRequest(reqBody)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	txnCmdRsp := &raftpb.RaftCommand{}
	if err = proto.Unmarshal(body, txnCmdRsp); err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusOK {
		fmt.Println("OK")
		return txnCmdRsp, nil
	}
	return nil, errors.New(string(body))
}

func (c *raftKVClient) txnToSingleCmd() error {
	cmd := c.txnCmds.Commands[0]
	switch cmd.Method {
	case common.DEL:
		return c.Delete(cmd.Key)
	case common.SET:
		return c.Set(cmd.Key, cmd.Value)
	default:
		return errors.New(("Not implemented"))
	}
}
