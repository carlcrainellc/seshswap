SeshSwap @VERSION@ (testnet) for Windows x64

Swap test ETH or test USDC for stagenet XMR, and rent Session Router exits by
the hour. Testnet only: Arbitrum Sepolia and Monero stagenet. Not audited.
Don't use it with real money.

Easiest: run SeshSwap-setup.exe. It installs Rusty (the Session Router
service) if it isn't there yet, keeping any key you already have, then this
wallet, with a Start menu entry.

This zip on its own
1. Install Rusty first (rusty-setup.exe from github.com/carlcrainellc/rusty-windows)
   and start its service once, in an admin PowerShell:
     sc.exe start RustySessionRouter
2. Unzip this folder anywhere and double-click seshswap.exe.

Your browser opens the wallet at http://127.0.0.1:7676. Make a wallet, write
down the 24 words, then press Get test coins. Keep the SeshSwap window open
while you use it; closing it (or Ctrl-C) quits.

How it uses Rusty
Private swaps and exits go through Rusty's Session Router service:
  RPC  tcp://127.0.0.1:1190   (map and unmap exits)
  DNS  127.0.0.1:53           (.sesh names)
Exit time is bought for Rusty's own .sesh address (C:\Rusty\my-client-address.txt).
Rusty handles routes and DNS while an exit is mapped and puts them back when
it is unmapped, when the service stops, or after a crash. The wallet never
changes Windows network settings itself.

Exits
On the Exits screen: Buy time, then Map (all traffic through the exit) and
Unmap. The panel at the top shows what Rusty has mapped right now and whether
the exit routes are up, including an exit mapped outside the wallet
(rusty-ctl). The wallet unmaps when you quit or lock it, and warns 5 minutes
before your paid time ends; at the end it unmaps by itself.
If SeshSwap is killed while an exit is mapped, Rusty keeps the mapping: open
SeshSwap and press Unmap, or run  C:\Rusty\rusty-ctl.exe unmap <exit>.sesh
(admin), or stop the service.

Other commands (in a terminal in this folder)
  seshswap.exe check     check monero-wallet-rpc and Rusty's service
  seshswap.exe stop      quit a running SeshSwap

Your wallet
Everything is in %LOCALAPPDATA%\SeshSwap: the encrypted wallet, the Monero
wallet files and logs. Settings are built in. To change one, copy
config.example.json to %LOCALAPPDATA%\SeshSwap\config.json and keep only the
lines you want to change.

What's inside, and the licences: THIRD-PARTY-NOTICES.txt
