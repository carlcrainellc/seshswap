// SPDX-License-Identifier: MIT
// TEST-ONLY PROTOTYPE. NOT AUDITED. DO NOT DEPLOY OR USE WITH REAL FUNDS.
//
// XmrSwapV2: Ethereum side of an ETH/ERC-20 <-> XMR atomic swap. One contract
// holds many swaps; each swap is stored as a single status byte keyed by the
// hash of its parameters (the parameters themselves live in calldata/events).
//
// Design credits (no code copied): two-timeout Ready/Claim/Refund protocol from
// AthanorLabs/atomic-swap docs/protocol.md; adaptor-free "reveal a key share"
// idea from Gugger, ePrint 2020/1126. Unlike AthanorLabs this contract checks
// the Monero (ed25519) public key directly, so no cross-curve DLEq proof is
// needed: the value committed here IS the counterparty's Monero key share.
//
// Roles
//   owner   ("Alice") locks ETH/ERC-20, wants XMR. May setReady and refund.
//   claimer ("Bob")   locks XMR, wants ETH. Claims directly or via a relayer
//                     using an EIP-712 signature (needs no ETH at all).
//
// Windows (exclusive on purpose; see DESIGN.md "why the windows never overlap")
//   [lock, t1)  PENDING : owner may setReady or refund.  claimer may not claim.
//   [lock, t2)  READY   : claimer may claim.             owner may not refund.
//   [t1,  t2)   PENDING : claimer may claim.             owner may not refund.
//   [t2,  ...)  any     : owner may refund.              claimer may not claim.
//   Both windows are at least MIN_D1 / MIN_D2 long (contract-enforced floor).
pragma solidity 0.8.26;

import {Ed25519} from "./Ed25519.sol";

interface IERC20Like {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address who) external view returns (uint256);
}

contract XmrSwapV2 {
    enum Status { NONE, PENDING, READY, CLAIMED, REFUNDED }

    struct Swap {
        address owner;       // Alice
        address claimer;     // Bob (EOA key that signs relayed claims)
        uint256 claimKey;    // Bob's Monero spend-key share, ed25519 point, LE-encoded integer
        uint256 refundKey;   // Alice's Monero spend-key share, same encoding
        uint64 t1;
        uint64 t2;
        address asset;       // address(0) = native ETH/xDAI
        uint256 value;
        bytes32 salt;        // makes ids unique; chosen by owner
    }

    bytes32 private constant EIP712_DOMAIN_TYPEHASH =
        keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)");
    bytes32 public constant CLAIM_TYPEHASH =
        keccak256("Claim(bytes32 swapId,address to,address relayer,uint256 fee)");
    uint256 private constant SECP256K1_N_HALF =
        0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0;
    uint256 private constant ED_IDENTITY = 1; // encoding of the neutral point (x=0, y=1)
    /// Shortest allowed windows. A claim or refund puts a key share in calldata, and
    /// that share is public from the moment the transaction is broadcast, even if it
    /// later reverts or is never mined. So each party must be able to act with a safety
    /// margin before its window closes; a 1-second window would make that impossible.
    /// The daemons (swapd) require much longer windows than these floors.
    uint64 public constant MIN_D1 = 30 minutes;
    uint64 public constant MIN_D2 = 30 minutes;

    bytes32 private immutable _domainSeparator;
    uint256 private immutable _chainId;

    mapping(bytes32 => Status) public status;
    /// payouts that could not be pushed (recipient rejected ETH, token paused/blacklisted)
    mapping(address => mapping(address => uint256)) public owed; // asset => account => amount

    event Locked(bytes32 indexed id, Swap swap);
    event Ready(bytes32 indexed id);
    event Claimed(bytes32 indexed id, uint256 secret, address to, address relayer, uint256 fee);
    event Refunded(bytes32 indexed id, uint256 secret);
    event PayoutDeferred(address indexed asset, address indexed to, uint256 amount);
    event Withdrawn(address indexed asset, address indexed from, address to, uint256 amount);

    error BadParams();
    error Exists();
    error WrongStatus();
    error NotAllowed();
    error WrongTime();
    error BadSecret();
    error BadSignature();
    error TransferFailed();
    error Reentrancy();

    constructor() {
        _chainId = block.chainid;
        _domainSeparator = _buildDomainSeparator();
    }

    // ------------------------------------------------------------ reentrancy (EIP-1153)
    modifier nonReentrant() {
        assembly ("memory-safe") {
            if tload(0) { mstore(0, 0xab143c06) revert(0x1c, 4) } // Reentrancy()
            tstore(0, 1)
        }
        _;
        assembly ("memory-safe") { tstore(0, 0) }
    }

    // ------------------------------------------------------------ views
    function swapId(Swap memory s) public pure returns (bytes32) {
        return keccak256(abi.encode(s));
    }

    function domainSeparator() public view returns (bytes32) {
        return block.chainid == _chainId ? _domainSeparator : _buildDomainSeparator();
    }

    function claimDigest(bytes32 id, address to, address relayer, uint256 fee) public view returns (bytes32) {
        bytes32 structHash = keccak256(abi.encode(CLAIM_TYPEHASH, id, to, relayer, fee));
        return keccak256(abi.encodePacked("\x19\x01", domainSeparator(), structHash));
    }

    /// true iff 0 < secret < L and secret*B (ed25519 base point) encodes exactly to `key`.
    /// `key` is the full 32-byte point encoding read as a little-endian integer
    /// (y | sign(x) << 255), so all 256 bits are compared; nothing is truncated or hashed.
    /// The keys are part of the Swap struct, which is hashed into the swap id, so a
    /// swap id binds both key shares, both parties, the asset, the value and t1/t2.
    function checkSecret(uint256 secret, uint256 key) public view returns (bool) {
        if (secret == 0 || secret >= Ed25519.L) return false;
        return Ed25519.mulBaseEncoded(secret) == key;
    }

    // ------------------------------------------------------------ owner (Alice)
    /// Lock funds. t1 = now + d1, t2 = t1 + d2.
    function lock(
        address claimer,
        uint256 claimKey,
        uint256 refundKey,
        uint64 d1,
        uint64 d2,
        address asset,
        uint256 value,
        bytes32 salt
    ) external payable nonReentrant returns (bytes32 id) {
        if (claimer == address(0) || value == 0 || d1 < MIN_D1 || d2 < MIN_D2) revert BadParams();
        if (claimKey == refundKey || claimKey == ED_IDENTITY || refundKey == ED_IDENTITY) revert BadParams();
        uint64 t1 = uint64(block.timestamp) + d1; // reverts on overflow
        Swap memory s = Swap(msg.sender, claimer, claimKey, refundKey, t1, t1 + d2, asset, value, salt);
        id = swapId(s);
        if (status[id] != Status.NONE) revert Exists();
        status[id] = Status.PENDING;

        if (asset == address(0)) {
            if (msg.value != value) revert BadParams();
        } else {
            if (msg.value != 0 || asset.code.length == 0) revert BadParams();
            uint256 before = IERC20Like(asset).balanceOf(address(this));
            if (!_callToken(asset, abi.encodeCall(IERC20Like.transferFrom, (msg.sender, address(this), value)))) {
                revert TransferFailed();
            }
            // Fee-on-transfer / deflationary tokens are rejected: the claimer would
            // otherwise reveal his key share and receive less than agreed.
            if (IERC20Like(asset).balanceOf(address(this)) - before != value) revert BadParams();
        }
        emit Locked(id, s);
    }

    /// Owner confirms the XMR is locked; from now on only the claimer can act until t2.
    /// It makes no external call, but it takes the lock anyway so that no token
    /// callback can ever interleave a status change with claim/refund/lock.
    function setReady(Swap calldata s) external nonReentrant {
        bytes32 id = swapId(s);
        if (status[id] != Status.PENDING) revert WrongStatus();
        if (msg.sender != s.owner) revert NotAllowed();
        if (block.timestamp >= s.t1) revert WrongTime();
        status[id] = Status.READY;
        emit Ready(id);
    }

    /// Owner takes the funds back by revealing her key share (lets Bob recover his XMR).
    function refund(Swap calldata s, uint256 secret) external nonReentrant {
        bytes32 id = swapId(s);
        Status st = status[id];
        if (st != Status.PENDING && st != Status.READY) revert WrongStatus();
        if (msg.sender != s.owner) revert NotAllowed();
        bool early = st == Status.PENDING && block.timestamp < s.t1;
        if (!early && block.timestamp < s.t2) revert WrongTime();
        if (!checkSecret(secret, s.refundKey)) revert BadSecret();
        status[id] = Status.REFUNDED;
        emit Refunded(id, secret);
        _pay(s.asset, s.owner, s.value);
    }

    // ------------------------------------------------------------ claimer (Bob)
    /// Claimer takes the funds by revealing his key share. `to` lets him receive
    /// at a fresh address (and survive a token blacklist on his own address).
    function claim(Swap calldata s, uint256 secret, address to) external nonReentrant {
        if (msg.sender != s.claimer) revert NotAllowed();
        _claim(s, secret, to, address(0), 0);
    }

    /// Anyone may submit a claim the claimer signed; the submitter is paid `fee`
    /// out of the swap value. If `relayer` is non-zero only that address may submit.
    /// The signature fixes the recipient and the fee, so a relayer (or anyone who
    /// copies the transaction) can neither redirect the funds nor raise the fee.
    function claimRelayed(
        Swap calldata s,
        uint256 secret,
        address to,
        address relayer,
        uint256 fee,
        uint8 v,
        bytes32 r,
        bytes32 sigS
    ) external nonReentrant {
        if (relayer != address(0) && msg.sender != relayer) revert NotAllowed();
        bytes32 id = swapId(s);
        bytes32 digest = claimDigest(id, to, relayer, fee);
        if (uint256(sigS) > SECP256K1_N_HALF || (v != 27 && v != 28)) revert BadSignature();
        address signer = ecrecover(digest, v, r, sigS);
        if (signer == address(0) || signer != s.claimer) revert BadSignature();
        _claim(s, secret, to, msg.sender, fee);
    }

    function _claim(Swap calldata s, uint256 secret, address to, address relayer, uint256 fee) private {
        bytes32 id = swapId(s);
        Status st = status[id];
        if (st != Status.PENDING && st != Status.READY) revert WrongStatus();
        if (block.timestamp >= s.t2) revert WrongTime();
        if (st == Status.PENDING && block.timestamp < s.t1) revert WrongTime();
        if (to == address(0) || fee > s.value) revert BadParams();
        if (!checkSecret(secret, s.claimKey)) revert BadSecret();
        status[id] = Status.CLAIMED;
        emit Claimed(id, secret, to, relayer, fee);
        _pay(s.asset, to, s.value - fee);
        _pay(s.asset, relayer, fee);
    }

    // ------------------------------------------------------------ pull fallback
    /// Withdraw a payout that could not be pushed. Sends to any address you choose.
    function withdraw(address asset, address to) external nonReentrant {
        uint256 amt = owed[asset][msg.sender];
        if (amt == 0 || to == address(0)) revert BadParams();
        owed[asset][msg.sender] = 0;
        emit Withdrawn(asset, msg.sender, to, amt);
        if (!_send(asset, to, amt)) revert TransferFailed();
    }

    // ------------------------------------------------------------ internals
    function _buildDomainSeparator() private view returns (bytes32) {
        return keccak256(abi.encode(EIP712_DOMAIN_TYPEHASH, keccak256("XmrSwapV2"), keccak256("1"),
                                    block.chainid, address(this)));
    }

    /// Pay out; never reverts once the secret is valid (a revert would publish
    /// the secret in a failed transaction and hand the other side both halves).
    function _pay(address asset, address to, uint256 value) private {
        if (value == 0) return;
        if (!_send(asset, to, value)) {
            owed[asset][to] += value;
            emit PayoutDeferred(asset, to, value);
        }
    }

    function _send(address asset, address to, uint256 value) private returns (bool ok) {
        if (asset == address(0)) {
            assembly ("memory-safe") {
                ok := call(gas(), to, value, 0, 0, 0, 0) // no return-data copy
            }
        } else {
            ok = _callToken(asset, abi.encodeCall(IERC20Like.transfer, (to, value)));
        }
    }

    /// Calls a token and reports success without ever reverting: accepts tokens
    /// that return nothing (USDT-style) or a non-zero word; copies at most 32
    /// bytes of return data (no return-data bombs).
    function _callToken(address token, bytes memory data) private returns (bool ok) {
        if (token.code.length == 0) return false;
        assembly ("memory-safe") {
            mstore(0, 0)
            ok := call(gas(), token, 0, add(data, 0x20), mload(data), 0, 0x20)
            if ok {
                let n := returndatasize()
                if n { ok := and(gt(n, 31), iszero(iszero(mload(0)))) }
            }
        }
    }
}
