// SPDX-License-Identifier: MIT
// TEST-ONLY. NOT AUDITED. TESTNETS ONLY. DO NOT USE WITH REAL FUNDS.
//
// SeshRegistry: a bonded, permissionless, on-chain directory of .sesh
// addresses. One listing format for everything (makers, sites, forums,
// stores, exits, ...). Nobody approves listings: post the bond and you are
// listed. There is no owner, admin, pause or upgrade.
//
//   * Every listing posts an ETH bond (makers a larger one).
//   * Makers: the bond caps how big a swap they may quote (bond x capFactor)
//     and backs a small penalty per swap. A taker who locks in XmrSwapV3
//     against a maker's signed commitment records it here; if the maker
//     never acknowledges it (on-chain ack) before `attestBy`, anyone can
//     slash the penalty to the taker. Same honest limit as the RFQ sketch:
//     Ethereum cannot see the Monero lock, so this slashes "no ack", not
//     "no XMR". A watchtower (the maker's own swapd, or anyone holding the
//     maker's signed ack) submits the ack.
//   * Other listings: slashable only through a challenge anyone can raise.
//     The owner must answer within `challengeWindow` or loses the bond to
//     the challenger. Answering only proves the owner key is alive; see
//     `challenge` for what this can and cannot judge.
//   * Leaving: requestExit, then withdraw after `exitDelay`. The bond can
//     still be slashed or challenged during the delay.
//   * Search happens off-chain from the events.
pragma solidity 0.8.26;

import {XmrSwapV3} from "../XmrSwapV3.sol";

interface IAggregatorV3Like {
    function latestRoundData() external view returns (uint80, int256, uint256, uint256, uint80);
    function decimals() external view returns (uint8);
}

contract SeshRegistry {
    // ------------------------------------------------------------ types
    enum Status {
        NONE,
        ACTIVE,
        EXITING,
        REMOVED,
        WITHDRAWN
    }
    enum CommitStage {
        NONE,
        OPEN,
        ACKED,
        RELEASED,
        SLASHED
    }

    struct Listing {
        address owner;
        Status status;
        uint64 exitAt; // EXITING: earliest withdraw
        bytes32 tag; // short category, e.g. "maker", "site"
        uint256 bond; // total ETH bond, including `reserved`
        uint256 reserved; // held against open maker commitments
        address challenger; // 0 = no open challenge
        uint64 challengeDeadline;
        uint64 openCommits;
        string sesh; // lowercase "<label>.sesh"
        string meta; // short description or a metadata URI
    }

    /// What a maker signs when it agrees a swap (EIP-712). It binds the V3
    /// swap's claimer and claim key, which are fresh per swap, so it cannot
    /// be reused for another swap.
    struct Commitment {
        uint256 listingId;
        address taker; // V3 owner
        address claimer; // V3 claimer (fresh per swap)
        uint256 claimKey; // V3 claimKey (maker's Monero key share)
        address asset; // address(0) = ETH, else `usdc`
        uint256 value;
        uint256 xmrAtomic; // what the maker promised (for the record)
        uint256 penalty; // ETH taken from the bond on a no-show
        uint64 attestWindow; // seconds after commit for the maker's ack
        uint64 expiry; // commit must be recorded by then
    }

    struct Commit {
        CommitStage stage;
        uint64 attestBy;
        uint256 listingId;
        address taker;
        uint256 penalty;
    }

    struct Info {
        address owner;
        Status status;
        bytes32 tag;
        uint256 bond;
        uint256 reserved;
        uint64 exitAt;
        address challenger;
        uint256 maxQuote;
    }

    struct Params {
        address swap; // XmrSwapV3
        uint256 siteMinBond; // PLACEHOLDER
        uint256 makerMinBond; // PLACEHOLDER
        uint256 capFactor; // PLACEHOLDER: max quote (ETH value) = bond * capFactor
        uint16 penaltyBps; // PLACEHOLDER: min penalty = ETH value * bps / 10000
        uint256 minPenalty; // PLACEHOLDER: floor in wei
        uint256 challengeBond; // PLACEHOLDER
        uint64 exitDelay; // PLACEHOLDER
        uint64 challengeWindow; // PLACEHOLDER
        uint64 minAttest; // PLACEHOLDER
        uint64 maxAttest; // PLACEHOLDER
        address usdc; // 0 = ETH swaps only
        uint8 usdcDecimals;
        address ethUsdFeed; // Chainlink-style ETH/USD, for the USDC cap
        uint64 maxFeedAge; // PLACEHOLDER
    }

    // ------------------------------------------------------------ constants
    bytes32 public constant MAKER = "maker";
    bytes32 private constant EIP712_DOMAIN_TYPEHASH =
        keccak256("EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)");
    bytes32 public constant COMMITMENT_TYPEHASH = keccak256(
        "Commitment(uint256 listingId,address taker,address claimer,uint256 claimKey,address asset,uint256 value,uint256 xmrAtomic,uint256 penalty,uint64 attestWindow,uint64 expiry)"
    );
    bytes32 public constant ACK_TYPEHASH = keccak256("RegistryAck(bytes32 swapId,uint256 listingId)");
    uint256 private constant SECP256K1_N_HALF = 0x7FFFFFFFFFFFFFFFFFFFFFFFFFFFFFFF5D576E7357A4501DDFE92F46681B20A0;
    uint256 public constant MAX_META = 280;
    uint256 public constant MAX_LABEL = 63;

    XmrSwapV3 public immutable swap;
    uint256 public immutable siteMinBond;
    uint256 public immutable makerMinBond;
    uint256 public immutable capFactor;
    uint16 public immutable penaltyBps;
    uint256 public immutable minPenalty;
    uint256 public immutable challengeBond;
    uint64 public immutable exitDelay;
    uint64 public immutable challengeWindow;
    uint64 public immutable minAttest;
    uint64 public immutable maxAttest;
    address public immutable usdc;
    uint8 public immutable usdcDecimals;
    address public immutable ethUsdFeed;
    uint64 public immutable maxFeedAge;
    bytes32 private immutable _domainSeparator;
    uint256 private immutable _chainId;

    uint256 public listingCount;
    mapping(uint256 => Listing) private _listings;
    /// keccak256(sesh) => listing id while ACTIVE or EXITING (no exact duplicates)
    mapping(bytes32 => uint256) public idOfSesh;
    /// V3 swap id => commitment record
    mapping(bytes32 => Commit) public commits;
    /// ETH that could not be pushed (recipient rejected it)
    mapping(address => uint256) public owed;

    // ------------------------------------------------------------ events
    event Listed(
        uint256 indexed id, address indexed owner, bytes32 indexed tag, string sesh, string meta, uint256 bond
    );
    event MetaUpdated(uint256 indexed id, string meta);
    event ToppedUp(uint256 indexed id, uint256 amount, uint256 bond);
    event ExitRequested(uint256 indexed id, uint64 exitAt);
    event ExitCancelled(uint256 indexed id);
    event Withdrawn(uint256 indexed id, address to, uint256 amount);
    event Challenged(uint256 indexed id, address indexed challenger, uint64 deadline);
    event ChallengeAnswered(uint256 indexed id, address challenger, uint256 paidToOwner);
    event ChallengeUpheld(uint256 indexed id, address challenger, uint256 bondPaid);
    event Committed(
        bytes32 indexed swapId,
        uint256 indexed listingId,
        address taker,
        uint256 value,
        uint256 penalty,
        uint64 attestBy
    );
    event Acked(bytes32 indexed swapId, uint256 indexed listingId);
    event Released(bytes32 indexed swapId, uint256 indexed listingId);
    event Slashed(bytes32 indexed swapId, uint256 indexed listingId, address taker, uint256 penalty);
    event PayoutDeferred(address indexed to, uint256 amount);
    event OwedWithdrawn(address indexed from, address to, uint256 amount);

    error BadParams();
    error BadName();
    error Taken();
    error LowBond();
    error NotOwner();
    error WrongStatus();
    error WrongTime();
    error NotMaker();
    error OverCap();
    error BadSignature();
    error Expired();
    error NotLocked();
    error Challenged_();
    error StaleFeed();
    error TransferFailed();
    error Reentrancy();

    constructor(Params memory p) {
        if (p.swap == address(0) || p.siteMinBond == 0 || p.makerMinBond < p.siteMinBond) revert BadParams();
        if (p.capFactor == 0 || p.penaltyBps == 0 || p.penaltyBps > 10_000 || p.challengeBond == 0) revert BadParams();
        if (p.exitDelay == 0 || p.challengeWindow == 0 || p.minAttest == 0 || p.maxAttest < p.minAttest) {
            revert BadParams();
        }
        if (
            p.usdc != address(0)
                && (p.ethUsdFeed == address(0) || p.usdcDecimals == 0 || p.usdcDecimals > 18 || p.maxFeedAge == 0)
        ) {
            revert BadParams();
        }
        swap = XmrSwapV3(p.swap);
        siteMinBond = p.siteMinBond;
        makerMinBond = p.makerMinBond;
        capFactor = p.capFactor;
        penaltyBps = p.penaltyBps;
        minPenalty = p.minPenalty;
        challengeBond = p.challengeBond;
        exitDelay = p.exitDelay;
        challengeWindow = p.challengeWindow;
        minAttest = p.minAttest;
        maxAttest = p.maxAttest;
        usdc = p.usdc;
        usdcDecimals = p.usdcDecimals;
        ethUsdFeed = p.ethUsdFeed;
        maxFeedAge = p.maxFeedAge;
        _chainId = block.chainid;
        _domainSeparator = _buildDomainSeparator();
    }

    modifier nonReentrant() {
        assembly ("memory-safe") {
            if tload(0) {
                mstore(0, 0xab143c06)
                revert(0x1c, 4)
            } // Reentrancy()
            tstore(0, 1)
        }
        _;
        assembly ("memory-safe") { tstore(0, 0) }
    }

    // ------------------------------------------------------------ views
    function listing(uint256 id) external view returns (Listing memory) {
        return _listings[id];
    }

    /// Fixed-size summary for clients (no strings).
    function info(uint256 id) external view returns (Info memory i) {
        Listing storage l = _listings[id];
        i.owner = l.owner;
        i.status = l.status;
        i.tag = l.tag;
        i.bond = l.bond;
        i.reserved = l.reserved;
        i.exitAt = l.exitAt;
        i.challenger = l.challenger;
        i.maxQuote = maxQuoteWei(id);
    }

    function minBondFor(bytes32 tag) public view returns (uint256) {
        return tag == MAKER ? makerMinBond : siteMinBond;
    }

    /// Bond not held against open commitments.
    function freeBond(uint256 id) public view returns (uint256) {
        Listing storage l = _listings[id];
        return l.bond - l.reserved;
    }

    /// Largest swap (in wei of ETH value) this maker may take right now.
    function maxQuoteWei(uint256 id) public view returns (uint256) {
        Listing storage l = _listings[id];
        if (l.tag != MAKER || l.status != Status.ACTIVE || l.bond < makerMinBond) return 0;
        return l.bond * capFactor;
    }

    /// ETH value (wei) of `value` units of `asset`. USDC uses the ETH/USD feed.
    function ethValue(address asset, uint256 value) public view returns (uint256) {
        if (asset == address(0)) return value;
        if (asset != usdc || usdc == address(0)) revert BadParams();
        (, int256 px,, uint256 updatedAt,) = IAggregatorV3Like(ethUsdFeed).latestRoundData();
        if (px <= 0 || updatedAt > block.timestamp || block.timestamp - updatedAt > maxFeedAge) revert StaleFeed();
        uint8 fd = IAggregatorV3Like(ethUsdFeed).decimals();
        // wei = value / 10^usdcDec (USD) / (answer / 10^fd) (ETH per USD) * 1e18
        return (value * (10 ** fd) * 1e18) / (10 ** usdcDecimals) / uint256(px);
    }

    /// Smallest penalty a commitment for this swap may carry.
    function requiredPenalty(address asset, uint256 value) public view returns (uint256) {
        uint256 p = (ethValue(asset, value) * penaltyBps) / 10_000;
        return p < minPenalty ? minPenalty : p;
    }

    function domainSeparator() public view returns (bytes32) {
        return block.chainid == _chainId ? _domainSeparator : _buildDomainSeparator();
    }

    function commitmentDigest(Commitment memory c) public view returns (bytes32) {
        return _digest(keccak256(abi.encode(COMMITMENT_TYPEHASH, c)));
    }

    function ackDigest(bytes32 swapId, uint256 listingId) public view returns (bytes32) {
        return _digest(keccak256(abi.encode(ACK_TYPEHASH, swapId, listingId)));
    }

    // ------------------------------------------------------------ listings
    /// Permissionless. `msg.value` is the bond.
    function register(string calldata sesh, bytes32 tag, string calldata meta)
        external
        payable
        nonReentrant
        returns (uint256 id)
    {
        if (tag == bytes32(0)) revert BadParams();
        if (bytes(meta).length > MAX_META) revert BadParams();
        _checkName(sesh);
        if (msg.value < minBondFor(tag)) revert LowBond();
        bytes32 h = keccak256(bytes(sesh));
        if (idOfSesh[h] != 0) revert Taken();
        id = ++listingCount;
        idOfSesh[h] = id;
        Listing storage l = _listings[id];
        l.owner = msg.sender;
        l.status = Status.ACTIVE;
        l.tag = tag;
        l.bond = msg.value;
        l.sesh = sesh;
        l.meta = meta;
        emit Listed(id, msg.sender, tag, sesh, meta, msg.value);
    }

    function updateMeta(uint256 id, string calldata meta) external {
        Listing storage l = _listings[id];
        if (msg.sender != l.owner) revert NotOwner();
        if (l.status != Status.ACTIVE) revert WrongStatus();
        if (bytes(meta).length > MAX_META) revert BadParams();
        l.meta = meta;
        emit MetaUpdated(id, meta);
    }

    /// Anyone may add to any active or exiting listing's bond.
    function topUp(uint256 id) external payable nonReentrant {
        Listing storage l = _listings[id];
        if (l.status != Status.ACTIVE && l.status != Status.EXITING) revert WrongStatus();
        if (msg.value == 0) revert BadParams();
        l.bond += msg.value;
        emit ToppedUp(id, msg.value, l.bond);
    }

    /// Hides the listing (wallets stop using it; makers take no new swaps) and
    /// starts the withdrawal delay. Slashes and challenges still apply.
    function requestExit(uint256 id) external {
        Listing storage l = _listings[id];
        if (msg.sender != l.owner) revert NotOwner();
        if (l.status != Status.ACTIVE) revert WrongStatus();
        l.status = Status.EXITING;
        l.exitAt = uint64(block.timestamp) + exitDelay;
        emit ExitRequested(id, l.exitAt);
    }

    function cancelExit(uint256 id) external {
        Listing storage l = _listings[id];
        if (msg.sender != l.owner) revert NotOwner();
        if (l.status != Status.EXITING) revert WrongStatus();
        if (l.bond < minBondFor(l.tag)) revert LowBond();
        l.status = Status.ACTIVE;
        l.exitAt = 0;
        emit ExitCancelled(id);
    }

    /// After the delay, with no open commitment and no open challenge.
    function withdraw(uint256 id, address to) external nonReentrant {
        Listing storage l = _listings[id];
        if (msg.sender != l.owner) revert NotOwner();
        if (to == address(0)) revert BadParams();
        if (l.status != Status.EXITING) revert WrongStatus();
        if (block.timestamp < l.exitAt) revert WrongTime();
        if (l.challenger != address(0)) revert Challenged_();
        if (l.openCommits != 0 || l.reserved != 0) revert WrongTime();
        uint256 amt = l.bond;
        l.bond = 0;
        l.status = Status.WITHDRAWN;
        delete idOfSesh[keccak256(bytes(l.sesh))];
        emit Withdrawn(id, to, amt);
        _push(to, amt);
    }

    // ------------------------------------------------------------ challenges
    /// Anyone may challenge an active or exiting listing by posting
    /// `challengeBond`. The owner must call `answer` within `challengeWindow`.
    ///
    /// What this can judge: whether the listing's owner key is still
    /// controlled by someone who cares (an abandoned listing is the common
    /// "dead" case, and its bond goes to whoever cleans it up).
    /// What it cannot judge: whether the .sesh site actually answers, or
    /// whether two different names are "the same site". Those happen
    /// off-chain and Ethereum cannot see them; there is no arbiter here by
    /// design. A live owner can keep a broken site listed by answering, and
    /// pays nothing for it except the gas; the challenger then loses the
    /// challenge bond to the owner (the price of griefing an active owner).
    /// Wallets do their own liveness check and hide listings that don't
    /// answer. Exact duplicates (same name) are refused at `register`.
    function challenge(uint256 id) external payable nonReentrant {
        Listing storage l = _listings[id];
        if (l.status != Status.ACTIVE && l.status != Status.EXITING) revert WrongStatus();
        if (l.challenger != address(0)) revert Challenged_();
        if (msg.value != challengeBond) revert BadParams();
        if (msg.sender == l.owner) revert NotOwner();
        l.challenger = msg.sender;
        l.challengeDeadline = uint64(block.timestamp) + challengeWindow;
        emit Challenged(id, msg.sender, l.challengeDeadline);
    }

    /// Owner proves the key is alive. The challenge bond goes to the owner.
    function answer(uint256 id) external nonReentrant {
        Listing storage l = _listings[id];
        if (msg.sender != l.owner) revert NotOwner();
        if (l.status != Status.ACTIVE && l.status != Status.EXITING) revert WrongStatus();
        address c = l.challenger;
        if (c == address(0)) revert WrongStatus();
        if (block.timestamp >= l.challengeDeadline) revert WrongTime();
        l.challenger = address(0);
        l.challengeDeadline = 0;
        emit ChallengeAnswered(id, c, challengeBond);
        _push(l.owner, challengeBond);
    }

    /// After the window with no answer, anyone may settle: the listing is
    /// removed and its free bond plus the challenge bond go to the
    /// challenger. Bond held for open maker commitments stays until those
    /// settle (takers keep their claim), then `sweepRemoved` sends what is
    /// left to the same challenger.
    function resolveChallenge(uint256 id) external nonReentrant {
        Listing storage l = _listings[id];
        address c = l.challenger;
        if (l.status != Status.ACTIVE && l.status != Status.EXITING) revert WrongStatus();
        if (c == address(0)) revert WrongStatus();
        if (block.timestamp < l.challengeDeadline) revert WrongTime();
        uint256 paid = l.bond - l.reserved;
        l.bond = l.reserved;
        l.status = Status.REMOVED; // `challenger` is kept as the beneficiary of sweepRemoved
        delete idOfSesh[keccak256(bytes(l.sesh))];
        emit ChallengeUpheld(id, c, paid);
        _push(c, paid + challengeBond);
    }

    // ------------------------------------------------------------ maker commitments
    /// Records a maker's signed commitment for a swap the taker already
    /// locked in XmrSwapV3. Anyone may submit it; the penalty always goes to
    /// the swap's owner (taker). Checks the bond cap and reserves the penalty.
    function commit(XmrSwapV3.Swap calldata s, Commitment calldata c, uint8 v, bytes32 r, bytes32 sigS)
        external
        nonReentrant
    {
        bytes32 id = swap.swapId(s);
        if (commits[id].stage != CommitStage.NONE) revert WrongStatus();
        (XmrSwapV3.Stage st,,) = swap.status(id);
        if (st != XmrSwapV3.Stage.PENDING) revert NotLocked();
        if (block.timestamp > c.expiry) revert Expired();
        if (
            s.owner != c.taker || s.claimer != c.claimer || s.claimKey != c.claimKey || s.asset != c.asset
                || s.value != c.value
        ) {
            revert BadParams();
        }
        if (c.attestWindow < minAttest || c.attestWindow > maxAttest) revert BadParams();
        uint64 attestBy = uint64(block.timestamp) + c.attestWindow;
        // The ack must be possible while the taker can still cancel at V3.
        if (attestBy >= s.t1) revert BadParams();

        Listing storage l = _listings[c.listingId];
        if (l.tag != MAKER) revert NotMaker();
        if (l.status != Status.ACTIVE || l.bond < makerMinBond) revert WrongStatus();
        _checkSig(commitmentDigest(c), l.owner, v, r, sigS);

        if (ethValue(c.asset, c.value) > l.bond * capFactor) revert OverCap();
        if (c.penalty < requiredPenalty(c.asset, c.value)) revert BadParams();
        if (l.bond - l.reserved < c.penalty) revert LowBond();

        l.reserved += c.penalty;
        l.openCommits += 1;
        commits[id] = Commit(CommitStage.OPEN, attestBy, c.listingId, s.owner, c.penalty);
        emit Committed(id, c.listingId, s.owner, c.value, c.penalty, attestBy);
    }

    /// "My XMR lock is broadcast." Signed by the listing owner; anyone may
    /// submit it (a watchtower does). Releases the reserved penalty. This is
    /// a signature, not a proof of a Monero output.
    function ack(bytes32 swapId, uint8 v, bytes32 r, bytes32 sigS) external nonReentrant {
        Commit storage c = commits[swapId];
        if (c.stage != CommitStage.OPEN) revert WrongStatus();
        Listing storage l = _listings[c.listingId];
        _checkSig(ackDigest(swapId, c.listingId), l.owner, v, r, sigS);
        c.stage = CommitStage.ACKED;
        _unreserve(l, c.penalty);
        emit Acked(swapId, c.listingId);
    }

    /// Without an ack, the reserve is released once the taker has confirmed
    /// the XMR (V3 READY) or the swap completed (V3 CLAIMED).
    function release(bytes32 swapId) external nonReentrant {
        Commit storage c = commits[swapId];
        if (c.stage != CommitStage.OPEN) revert WrongStatus();
        (XmrSwapV3.Stage st,,) = swap.status(swapId);
        if (st != XmrSwapV3.Stage.READY && st != XmrSwapV3.Stage.CLAIMED) revert WrongStatus();
        c.stage = CommitStage.RELEASED;
        _unreserve(_listings[c.listingId], c.penalty);
        emit Released(swapId, c.listingId);
    }

    /// No ack by `attestBy`: anyone may pay the penalty to the taker. Not
    /// possible once the taker confirmed the XMR (READY) or the swap was
    /// claimed, since then the maker evidently followed through.
    function slash(bytes32 swapId) external nonReentrant {
        Commit storage c = commits[swapId];
        if (c.stage != CommitStage.OPEN) revert WrongStatus();
        if (block.timestamp < c.attestBy) revert WrongTime();
        (XmrSwapV3.Stage st,,) = swap.status(swapId);
        if (st == XmrSwapV3.Stage.READY || st == XmrSwapV3.Stage.CLAIMED) revert WrongStatus();
        c.stage = CommitStage.SLASHED;
        Listing storage l = _listings[c.listingId];
        l.reserved -= c.penalty;
        l.bond -= c.penalty;
        l.openCommits -= 1;
        emit Slashed(swapId, c.listingId, c.taker, c.penalty);
        _push(c.taker, c.penalty);
    }

    /// After a challenge removed a maker listing, bond that was held for its
    /// open commitments goes to the challenger once they have all settled.
    function sweepRemoved(uint256 id) external nonReentrant {
        Listing storage l = _listings[id];
        if (l.status != Status.REMOVED || l.openCommits != 0 || l.bond == 0) revert WrongStatus();
        uint256 amt = l.bond;
        l.bond = 0;
        emit Withdrawn(id, l.challenger, amt);
        _push(l.challenger, amt);
    }

    function withdrawOwed(address to) external nonReentrant {
        uint256 amt = owed[msg.sender];
        if (amt == 0 || to == address(0)) revert BadParams();
        owed[msg.sender] = 0;
        emit OwedWithdrawn(msg.sender, to, amt);
        (bool ok,) = to.call{value: amt}("");
        if (!ok) revert TransferFailed();
    }

    // ------------------------------------------------------------ internals
    function _unreserve(Listing storage l, uint256 penalty) private {
        l.reserved -= penalty;
        l.openCommits -= 1;
    }

    function _push(address to, uint256 amt) private {
        if (amt == 0) return;
        (bool ok,) = to.call{value: amt, gas: 30_000}("");
        if (!ok) {
            owed[to] += amt;
            emit PayoutDeferred(to, amt);
        }
    }

    /// "<label>.sesh", label 1..63 of [a-z0-9-], not starting or ending with '-'.
    function _checkName(string calldata sesh) private pure {
        bytes calldata b = bytes(sesh);
        uint256 n = b.length;
        if (n < 6 || n > MAX_LABEL + 5) revert BadName();
        if (b[n - 5] != "." || b[n - 4] != "s" || b[n - 3] != "e" || b[n - 2] != "s" || b[n - 1] != "h") {
            revert BadName();
        }
        uint256 L = n - 5;
        for (uint256 i = 0; i < L; i++) {
            bytes1 ch = b[i];
            bool ok = (ch >= "a" && ch <= "z") || (ch >= "0" && ch <= "9") || (ch == "-" && i != 0 && i != L - 1);
            if (!ok) revert BadName();
        }
    }

    function _digest(bytes32 structHash) private view returns (bytes32) {
        return keccak256(abi.encodePacked("\x19\x01", domainSeparator(), structHash));
    }

    function _buildDomainSeparator() private view returns (bytes32) {
        return keccak256(
            abi.encode(EIP712_DOMAIN_TYPEHASH, keccak256("SeshRegistry"), keccak256("1"), block.chainid, address(this))
        );
    }

    function _checkSig(bytes32 digest, address signer, uint8 v, bytes32 r, bytes32 sigS) private pure {
        if (uint256(sigS) > SECP256K1_N_HALF || (v != 27 && v != 28)) revert BadSignature();
        address a = ecrecover(digest, v, r, sigS);
        if (a == address(0) || a != signer) revert BadSignature();
    }
}
