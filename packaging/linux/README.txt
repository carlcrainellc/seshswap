SeshSwap (testnet) for Linux x86_64

Swap test ETH or test USDC for stagenet XMR. Testnet only: Arbitrum Sepolia
and Monero stagenet. Not audited. Don't use it with real money.

Start
  tar xzf seshswap-*-linux-x86_64.tar.gz
  cd seshswap-*-linux-x86_64
  ./seshswap

Your browser opens the wallet at http://127.0.0.1:7676. Make a wallet, write
down the 24 words, then press Get test coins. Keep the terminal open while
you use it; Ctrl-C quits.

Private swaps (once)
Swaps reach the XMR seller over Session Router. It needs permission to make
its own network adapter. Run this once (it asks for your password):
  ./seshswap setup-router
If your account can't use sudo, run ./seshswap check once, then someone
who can runs this in the folder:
  sudo setcap cap_net_admin+ep session-router/bin/session-router
The router still runs as you, not root. Balances and Get test coins work
without this step. If you move the folder, run it again.

Menu entry (optional)
  ./install.sh
Copies SeshSwap to ~/.local/share/seshswap and adds it to your app menu. No
sudo. Then run `seshswap setup-router` once. ./install.sh --uninstall takes it
out again (your wallet in ~/.seshswap stays).

Other commands
  ./seshswap stop      quit a SeshSwap started from the menu
  ./seshswap check     check that the bundled programs run here

Your wallet
Everything is in ~/.seshswap: the encrypted wallet, the Monero wallet files
and logs (~/.seshswap/logs). Settings are built in. To change one, copy
config.example.json to ~/.seshswap/config.json and keep only the lines you
want to change.

Exits
Exit Markets connects as a full tunnel and needs the setup-router step
above. Pointing system DNS at the exit needs a sudo rule; see EXITS.md in
the repository. Without it the exit still works and DNS stays as it was.

Works on Ubuntu 22.04 and 24.04 (x86_64). Linux only for now.

What's inside, and the licences: THIRD-PARTY-NOTICES.txt
