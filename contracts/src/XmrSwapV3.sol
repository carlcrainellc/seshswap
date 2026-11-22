// SPDX-License-Identifier: MIT
// TEST-ONLY PROTOTYPE. NOT AUDITED. DO NOT DEPLOY OR USE WITH REAL FUNDS.
//
// XmrSwapV3: Ethereum side of an ETH/ERC-20 <-> XMR atomic swap. One contract
// holds many swaps; each swap is stored in one storage slot keyed by the hash of
// its parameters (the parameters themselves live in calldata/events).
//
// What V3 adds to V2 (see DESIGN.md):
//
// 1. "Freeze, then reveal". A claim or an early cancel is two transactions:
//      an INTENT that carries no secret and freezes the other side, then
//      the REVEAL (claim / refund with the secret), sent only once the intent is final.
//    While a claim intent is open (stage CLAIMING, until `deadline`) the owner cannot
//    refund; while a cancel intent is open (CANCELLING, until `deadline`) the claimer
//    cannot claim. So a secret is only ever published while the other side is
//    locked out for at least `grace` seconds. Censorship, a slow relayer or a late
//    daemon can then only cause a refund, never a theft. A late refund at/after t2
//    stays one step: from t2 on the claimer can never act again.
//
// 2. A taker premium against a signed "XMR locked" acknowledgement. The owner may
//    lock a small native-coin `premium` next to the swap value. After locking its
//    XMR the claimer may `ackLocked` (EIP-712 signature by the claimer key; anyone
//    may submit it), naming where the premium goes. If the owner then cancels
//    (refund that settles a cancel intent), the premium goes to that address. In
//    every other outcome it goes back to the owner: claim, a late refund at/after
//    t2, a refund after an unrevealed claim intent, or a cancel without an ack.
//
// Roles
//   owner   ("Alice") locks ETH/ERC-20 (+ premium), wants XMR.
//   claimer ("Bob")   locks XMR, wants ETH. Signs intents/claims (needs no ETH).
//
// Who may do what (exclusive at every moment; tested by fuzzing):
//   PENDING, now < t1          owner: setReady                    claimer: ackLocked
//   PENDING, lockIn <= now < t1 owner: cancelIntent (lockIn = lock time + LOCK_IN)
//   READY,   now < t2          claimer: claimIntent
//   PENDING, t1 <= now < t2    claimer: claimIntent
//   CANCELLING, now < deadline owner: refund (cancel)
//   CANCELLING, deadline <= now: claimer: claimIntent if t1 <= now < t2; owner: refund if now >= t2
//   CLAIMING, now < deadline   claimer: claim (reveal)
//   CLAIMING, now >= deadline  owner: refund (the claimer never revealed)
//   PENDING/READY, now >= t2   owner: refund
//   deadline(CLAIMING)   = max(t2, intent time + grace)
//   deadline(CANCELLING) = intent time + grace
//   deadline(PENDING/READY) = lock time + LOCK_IN (the lock-in window, see cancelIntent)
//
// Design credits (no code copied): two-timeout Ready/Claim/Refund protocol from
// AthanorLabs/atomic-swap docs/protocol.md; adaptor-free "reveal a key share"
// idea from Gugger, ePrint 2020/1126. The contract checks the Monero (ed25519)
// public key directly, so no cross-curve DLEq proof is needed.
pragma solidity 0.8.26;

import {Ed25519} from "./Ed25519.sol";

interface IERC20Like {
    function transfer(address to, uint256 amount) external returns (bool);
    function transferFrom(address from, address to, uint256 amount) external returns (bool);
    function balanceOf(address who) external view returns (uint256);
}

contract XmrSwapV3 {
    enum Stage { NONE, PENDING, READY, CLAIMED, REFUNDED, CLAIMING, CANCELLING }

    struct Swap {
        address owner;       // Alice
        address claimer;     // Bob (EOA key that signs intents and relayed claims)
        uint256 claimKey;    // Bob's Monero spend-key share, ed25519 point, LE-encoded integer
        uint256 refundKey;   // Alice's Monero spend-key share, same encoding
        uint64 t1;
        uint64 t2;
        uint64 grace;        // how long an intent freezes the other side
        address asset;       // address(0) = native coin
        uint256 value;
        uint256 premium;     // native coin; 0 = none
        bytes32 salt;        // makes ids unique; chosen by owner
    }

    /// One storage slot per swap.
    struct Status {
        Stage stage;
        uint64 deadline;     // end of the freeze while CLAIMING / CANCELLING; end of the lock-in while PENDING / READY
        address premiumTo;   // set by ackLocked; 0 = no ack
    }

    bytes32 private constant EIP712_DOMAIN_TYPEHASH =
        keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)");
    bytes32 public constant CLAIM_TYPEHASH =
        keccak256("Claim(bytes32 swapId,address to,address relayer,uint256 fee)");
    bytes32 public constant INTENT_TYPEHASH = keccak256("ClaimIntent(bytes32 swapId)");
    bytes32 public constant ACK_TYPEHASH = keccak256("AckLocked(bytes32 swapId,address premiumTo)");
    uint256 private constant SECP256K1_N_HALF =
        0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0;
    uint256 private constant ED_IDENTITY = 1; // encoding of the neutral point (x=0, y=1)
    /// Shortest allowed windows (same floors as V2). The daemons require more.
    uint64 public constant MIN_D1 = 30 minutes;
    uint64 public constant MIN_D2 = 30 minutes;
    /// Bounds on the freeze. The reveal must be mined within `grace` of its intent;
    /// a short grace would bring back the race the intent exists to remove, and a
    /// very long one would let either side delay the other for too long.
    uint64 public constant MIN_GRACE = 30 minutes;
    uint64 public constant MAX_GRACE = 48 hours;
    /// Lock-in window: for this long after lock() the owner cannot cancelIntent,
    /// so a claimer who locks XMR and acks inside it can't be front-run by a
    /// cancel that keeps the premium. Fixed (nobody can extend it) and shorter
    /// than MIN_D1, so it always ends before t1 and never touches refunds.
    uint64 public constant LOCK_IN = 20 minutes;

    bytes32 private immutable _domainSeparator;
    uint256 private immutable _chainId;

    mapping(bytes32 => Status) public status;
    /// payouts that could not be pushed (recipient rejected ETH, token paused/blacklisted)
    mapping(address => mapping(address => uint256)) public owed; // asset => account => amount

    event Locked(bytes32 indexed id, Swap swap);
    event Ready(bytes32 indexed id);
    event Acked(bytes32 indexed id, address premiumTo);
    event ClaimIntent(bytes32 indexed id, uint64 deadline);
    event CancelIntent(bytes32 indexed id, uint64 deadline);
    event Claimed(bytes32 indexed id, uint256 secret, address to, address relayer, uint256 fee);
    event Refunded(bytes32 indexed id, uint256 secret);
    event PremiumPaid(bytes32 indexed id, address to, uint256 amount);
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

    function _digest(bytes32 structHash) private view returns (bytes32) {
        return keccak256(abi.encodePacked("\x19\x01", domainSeparator(), structHash));
    }

    function claimDigest(bytes32 id, address to, address relayer, uint256 fee) public view returns (bytes32) {
        return _digest(keccak256(abi.encode(CLAIM_TYPEHASH, id, to, relayer, fee)));
    }

    function intentDigest(bytes32 id) public view returns (bytes32) {
        return _digest(keccak256(abi.encode(INTENT_TYPEHASH, id)));
    }

    function ackDigest(bytes32 id, address premiumTo) public view returns (bytes32) {
        return _digest(keccak256(abi.encode(ACK_TYPEHASH, id, premiumTo)));
    }

    /// true iff 0 < secret < L and secret*B (ed25519 base point) encodes exactly to `key`.
    /// `key` is the full 32-byte point encoding read as a little-endian integer
    /// (y | sign(x) << 255), so all 256 bits are compared; nothing is truncated or hashed.
    /// The keys are part of the Swap struct, which is hashed into the swap id, so a
    /// swap id binds both key shares, both parties, asset, value, premium, t1/t2 and grace.
    function checkSecret(uint256 secret, uint256 key) public view returns (bool) {
        if (secret == 0 || secret >= Ed25519.L) return false;
        return Ed25519.mulBaseEncoded(secret) == key;
    }

    /// The claimer may send a claim intent now.
    function claimerWindow(Swap calldata s, Status memory st) public view returns (bool) {
        uint256 t = block.timestamp;
        if (t >= s.t2) return false;
        if (st.stage == Stage.READY) return true;
        if (st.stage == Stage.PENDING) return t >= s.t1;
        if (st.stage == Stage.CANCELLING) return t >= s.t1 && t >= st.deadline;
        return false;
    }

    // ------------------------------------------------------------ owner (Alice)
    /// Lock funds (and an optional native-coin premium). t1 = now + d1, t2 = t1 + d2.
    /// msg.value must be value + premium for native-coin swaps, and premium for ERC-20 swaps.
    function lock(
        address claimer,
        uint256 claimKey,
        uint256 refundKey,
        uint64 d1,
        uint64 d2,
        uint64 grace,
        address asset,
        uint256 value,
        uint256 premium,
        bytes32 salt
    ) external payable nonReentrant returns (bytes32 id) {
        if (claimer == address(0) || value == 0 || d1 < MIN_D1 || d2 < MIN_D2) revert BadParams();
        if (grace < MIN_GRACE || grace > MAX_GRACE) revert BadParams();
        if (claimKey == refundKey || claimKey == ED_IDENTITY || refundKey == ED_IDENTITY) revert BadParams();
        uint64 t1 = uint64(block.timestamp) + d1; // reverts on overflow
        Swap memory s = Swap(msg.sender, claimer, claimKey, refundKey, t1, t1 + d2, grace, asset, value, premium, salt);
        id = swapId(s);
        if (status[id].stage != Stage.NONE) revert Exists();
        status[id] = Status(Stage.PENDING, uint64(block.timestamp) + LOCK_IN, address(0));

        if (asset == address(0)) {
            if (msg.value != value + premium) revert BadParams(); // reverts on overflow
        } else {
            if (msg.value != premium || asset.code.length == 0) revert BadParams();
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
    /// callback can ever interleave a status change with the other functions.
    function setReady(Swap calldata s) external nonReentrant {
        bytes32 id = swapId(s);
        Status storage st = status[id];
        if (st.stage != Stage.PENDING) revert WrongStatus();
        if (msg.sender != s.owner) revert NotAllowed();
        if (block.timestamp >= s.t1) revert WrongTime();
        st.stage = Stage.READY;
        emit Ready(id);
    }

    /// Step 1 of an early cancel: freezes the claimer for `grace`. Carries no secret.
    /// Only once per swap, only while PENDING, from the end of the lock-in window
    /// (lock time + LOCK_IN, kept in `deadline`) until t1.
    function cancelIntent(Swap calldata s) external nonReentrant {
        bytes32 id = swapId(s);
        Status storage st = status[id];
        if (st.stage != Stage.PENDING) revert WrongStatus();
        if (msg.sender != s.owner) revert NotAllowed();
        if (block.timestamp >= s.t1 || block.timestamp < st.deadline) revert WrongTime();
        uint64 d = uint64(block.timestamp) + s.grace;
        st.stage = Stage.CANCELLING;
        st.deadline = d;
        emit CancelIntent(id, d);
    }

    /// The owner takes the funds back by revealing her key share (lets Bob recover his XMR).
    ///   - CANCELLING, before its deadline (step 2 of a cancel), or any time from t2 on
    ///   - CLAIMING, from its deadline on (the claimer never revealed)
    ///   - PENDING / READY, from t2 on
    function refund(Swap calldata s, uint256 secret) external nonReentrant {
        bytes32 id = swapId(s);
        Status memory st = status[id];
        if (msg.sender != s.owner) revert NotAllowed();
        uint256 t = block.timestamp;
        bool ok = false;
        if (st.stage == Stage.CANCELLING) ok = t < st.deadline || t >= s.t2;
        else if (st.stage == Stage.CLAIMING) ok = t >= st.deadline;
        else if (st.stage == Stage.PENDING || st.stage == Stage.READY) ok = t >= s.t2;
        else revert WrongStatus();
        if (!ok) revert WrongTime();
        if (!checkSecret(secret, s.refundKey)) revert BadSecret();
        status[id] = Status(Stage.REFUNDED, 0, address(0));
        // The premium is forfeited only when this refund settles a cancel intent made after an ack.
        address premiumDest = st.stage == Stage.CANCELLING && st.premiumTo != address(0) ? st.premiumTo : s.owner;
        emit Refunded(id, secret);
        _pay(s.asset, s.owner, s.value);
        _payPremium(id, premiumDest, s.premium);
    }

    // ------------------------------------------------------------ claimer (Bob)
    /// "My XMR is locked." Names where the premium goes if the owner cancels after
    /// this. Signed by the claimer key (EIP-712); anyone may submit it. Only once,
    /// only while PENDING and before t1, only with a premium.
    function ackLocked(Swap calldata s, address to, uint8 v, bytes32 r, bytes32 sigS) external nonReentrant {
        bytes32 id = swapId(s);
        Status storage st = status[id];
        if (st.stage != Stage.PENDING) revert WrongStatus();
        if (block.timestamp >= s.t1) revert WrongTime();
        if (to == address(0) || s.premium == 0) revert BadParams();
        if (st.premiumTo != address(0)) revert Exists();
        _checkSig(ackDigest(id, to), s.claimer, v, r, sigS);
        st.premiumTo = to;
        emit Acked(id, to);
    }

    /// Step 1 of a claim: freezes the owner until max(t2, now + grace). Carries no
    /// secret. Signed by the claimer key (EIP-712); anyone may submit it.
    function claimIntent(Swap calldata s, uint8 v, bytes32 r, bytes32 sigS) external nonReentrant {
        bytes32 id = swapId(s);
        Status storage st = status[id];
        if (st.stage == Stage.NONE || st.stage == Stage.CLAIMED || st.stage == Stage.REFUNDED
            || st.stage == Stage.CLAIMING) revert WrongStatus();
        if (!claimerWindow(s, st)) revert WrongTime();
        _checkSig(intentDigest(id), s.claimer, v, r, sigS);
        uint64 d = uint64(block.timestamp) + s.grace;
        if (d < s.t2) d = s.t2;
        st.stage = Stage.CLAIMING;
        st.deadline = d;
        emit ClaimIntent(id, d);
    }

    /// Step 2: the claimer takes the funds by revealing his key share. `to` lets him
    /// receive at a fresh address (and survive a token blacklist on his own address).
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
        _checkSig(claimDigest(swapId(s), to, relayer, fee), s.claimer, v, r, sigS);
        _claim(s, secret, to, msg.sender, fee);
    }

    function _claim(Swap calldata s, uint256 secret, address to, address relayer, uint256 fee) private {
        bytes32 id = swapId(s);
        Status memory st = status[id];
        if (st.stage != Stage.CLAIMING) revert WrongStatus();
        if (block.timestamp >= st.deadline) revert WrongTime();
        if (to == address(0) || fee > s.value) revert BadParams();
        if (!checkSecret(secret, s.claimKey)) revert BadSecret();
        status[id] = Status(Stage.CLAIMED, 0, address(0));
        emit Claimed(id, secret, to, relayer, fee);
        _pay(s.asset, to, s.value - fee);
        _pay(s.asset, relayer, fee);
        _payPremium(id, s.owner, s.premium); // the owner completed the swap: premium back
    }

    function _checkSig(bytes32 digest, address signer, uint8 v, bytes32 r, bytes32 sigS) private pure {
        if (uint256(sigS) > SECP256K1_N_HALF || (v != 27 && v != 28)) revert BadSignature();
        address a = ecrecover(digest, v, r, sigS);
        if (a == address(0) || a != signer) revert BadSignature();
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
        return keccak256(abi.encode(EIP712_DOMAIN_TYPEHASH, keccak256("XmrSwapV3"), keccak256("1"),
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

    function _payPremium(bytes32 id, address to, uint256 amount) private {
        if (amount == 0) return;
        emit PremiumPaid(id, to, amount);
        _pay(address(0), to, amount);
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
