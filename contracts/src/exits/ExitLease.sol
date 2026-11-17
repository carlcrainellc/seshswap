// SPDX-License-Identifier: MIT
// TEST-ONLY. NOT AUDITED. TESTNETS ONLY. DO NOT USE WITH REAL FUNDS.
//
// ExitLease: buy hours on a Session Router exit listed in SeshRegistry.
//
//   * A seller lists its exit's .sesh in SeshRegistry with tag "exit" (the
//     small site-style bond), then sets an offer here: payout address, price
//     per hour in ETH and/or USDC, and the most hours one purchase may buy.
//     Only the listing's owner can set or clear the offer.
//   * A buyer calls buy(): it pays price x hours straight to the payout
//     address in the same transaction (this contract never holds funds) and
//     the lease is recorded: buyer, seller payout, asset, amount, start, end
//     and the buyer's own .sesh address (the address the exit admits).
//   * Buying again for the same listing and .sesh before the time runs out
//     adds the hours after the current end. Nothing renews by itself.
//   * Paid hours are final. There is no refund, no admin, no pause, no fee
//     and no upgrade.
//
// The seller's watcher admits the buyer's .sesh from start to end when it sees
// the Leased event. Ethereum cannot see the exit: an exit that takes the money
// and never admits is only answerable through the registry's challenge and the
// seller's reputation, not here.
pragma solidity 0.8.26;

import {SeshRegistry} from "../registry/SeshRegistry.sol";

interface IERC20Like {
    function transferFrom(address from, address to, uint256 value) external returns (bool);
}

contract ExitLease {
    bytes32 public constant EXIT = "exit";
    uint32 public constant MAX_HOURS_CAP = 24 * 31; // one purchase: at most 31 days
    uint256 public constant SESH_LEN = 57; // 52 z-base32 chars + ".sesh"

    struct Offer {
        address owner; // listing owner when the offer was set
        address payout; // where payments go
        uint128 weiPerHour; // 0 = ETH not accepted
        uint128 usdcPerHour; // 0 = USDC not accepted (6-decimal units)
        uint32 maxHours; // per purchase
    }

    struct Lease {
        uint256 listingId;
        address buyer;
        address payout; // the seller's payout address at purchase time
        address asset; // address(0) = ETH, else `usdc`
        uint256 amount;
        uint64 start;
        uint64 end;
        string buyerSesh;
    }

    SeshRegistry public immutable registry;
    address public immutable usdc;

    mapping(uint256 => Offer) public offers; // listing id => offer
    Lease[] private _leases; // lease id = index
    /// listing id => keccak256(buyer .sesh) => end of the paid time
    mapping(uint256 => mapping(bytes32 => uint64)) public paidUntil;

    uint256 private _lock = 1;

    event OfferSet(
        uint256 indexed listingId,
        address indexed owner,
        address payout,
        uint128 weiPerHour,
        uint128 usdcPerHour,
        uint32 maxHours
    );
    event OfferCleared(uint256 indexed listingId);
    event Leased(
        uint256 indexed leaseId,
        uint256 indexed listingId,
        address indexed buyer,
        address payout,
        address asset,
        uint256 amount,
        uint32 hoursBought,
        uint64 start,
        uint64 end,
        string buyerSesh
    );

    error BadParams();
    error BadSesh();
    error NotOwner();
    error NotExit();
    error NoOffer();
    error AssetNotAccepted();
    error TooManyHours();
    error WrongAmount();
    error TransferFailed();
    error Reentrancy();

    modifier nonReentrant() {
        if (_lock != 1) revert Reentrancy();
        _lock = 2;
        _;
        _lock = 1;
    }

    constructor(SeshRegistry registry_, address usdc_) {
        if (address(registry_) == address(0)) revert BadParams();
        registry = registry_;
        usdc = usdc_;
    }

    // ------------------------------------------------------------ seller
    function setOffer(uint256 listingId, address payout, uint128 weiPerHour, uint128 usdcPerHour, uint32 maxHours)
        external
    {
        SeshRegistry.Info memory i = registry.info(listingId);
        if (msg.sender != i.owner) revert NotOwner();
        if (i.tag != EXIT || i.status != SeshRegistry.Status.ACTIVE) revert NotExit();
        if (payout == address(0) || maxHours == 0 || maxHours > MAX_HOURS_CAP) revert BadParams();
        if (weiPerHour == 0 && usdcPerHour == 0) revert BadParams();
        if (usdcPerHour != 0 && usdc == address(0)) revert BadParams();
        offers[listingId] = Offer(msg.sender, payout, weiPerHour, usdcPerHour, maxHours);
        emit OfferSet(listingId, msg.sender, payout, weiPerHour, usdcPerHour, maxHours);
    }

    function clearOffer(uint256 listingId) external {
        if (msg.sender != registry.info(listingId).owner) revert NotOwner();
        delete offers[listingId];
        emit OfferCleared(listingId);
    }

    // ------------------------------------------------------------ buyer
    /// Price for `hoursBought` hours of `listingId` in `asset` (reverts if not for sale).
    function quote(uint256 listingId, address asset, uint32 hoursBought) public view returns (uint256) {
        Offer memory o = _liveOffer(listingId);
        if (hoursBought == 0) revert BadParams();
        if (hoursBought > o.maxHours) revert TooManyHours();
        uint256 per;
        if (asset == address(0)) per = o.weiPerHour;
        else if (asset == usdc && usdc != address(0)) per = o.usdcPerHour;
        if (per == 0) revert AssetNotAccepted();
        return per * hoursBought;
    }

    /// Pay for `hoursBought` hours. `expectedAmount` must equal the current
    /// quote, so a price change between quote and buy reverts instead of
    /// charging more. ETH: send exactly that as msg.value. USDC: approve it first.
    function buy(
        uint256 listingId,
        address asset,
        uint32 hoursBought,
        uint256 expectedAmount,
        string calldata buyerSesh
    ) external payable nonReentrant returns (uint256 leaseId) {
        _checkSesh(buyerSesh);
        uint256 amount = quote(listingId, asset, hoursBought);
        if (amount != expectedAmount) revert WrongAmount();
        if (asset == address(0)) {
            if (msg.value != amount) revert WrongAmount();
        } else if (msg.value != 0) {
            revert WrongAmount();
        }
        Lease memory l = _record(listingId, asset, amount, hoursBought, buyerSesh);
        leaseId = _leases.length - 1;
        emit Leased(
            leaseId, listingId, msg.sender, l.payout, asset, amount, hoursBought, l.start, l.end, buyerSesh
        );
        _pay(asset, l.payout, amount);
    }

    // ------------------------------------------------------------ views
    function leaseCount() external view returns (uint256) {
        return _leases.length;
    }

    function lease(uint256 leaseId) external view returns (Lease memory) {
        return _leases[leaseId];
    }

    /// End of the paid time for this .sesh on this listing (0 = never bought).
    function activeUntil(uint256 listingId, string calldata buyerSesh) external view returns (uint64) {
        return paidUntil[listingId][keccak256(bytes(buyerSesh))];
    }

    // ------------------------------------------------------------ internal
    function _record(uint256 listingId, address asset, uint256 amount, uint32 hoursBought, string calldata buyerSesh)
        private
        returns (Lease memory l)
    {
        bytes32 key = keccak256(bytes(buyerSesh));
        uint64 start = paidUntil[listingId][key];
        if (start < block.timestamp) start = uint64(block.timestamp);
        uint64 end = start + uint64(hoursBought) * 1 hours;
        paidUntil[listingId][key] = end;
        l = Lease(listingId, msg.sender, offers[listingId].payout, asset, amount, start, end, buyerSesh);
        _leases.push(l);
    }

    function _pay(address asset, address payout, uint256 amount) private {
        if (asset == address(0)) {
            (bool ok,) = payout.call{value: amount}("");
            if (!ok) revert TransferFailed();
        } else {
            if (!IERC20Like(asset).transferFrom(msg.sender, payout, amount)) revert TransferFailed();
        }
    }

    function _liveOffer(uint256 listingId) private view returns (Offer memory o) {
        o = offers[listingId];
        if (o.payout == address(0)) revert NoOffer();
        SeshRegistry.Info memory i = registry.info(listingId);
        // A listing that is leaving, removed or withdrawn is not for sale, and an
        // offer only counts while its setter still owns the listing.
        if (i.tag != EXIT || i.status != SeshRegistry.Status.ACTIVE) revert NotExit();
        if (i.owner != o.owner) revert NoOffer();
    }

    /// A Session Router client address: 52 z-base32 characters + ".sesh".
    function _checkSesh(string calldata s) private pure {
        bytes calldata b = bytes(s);
        if (b.length != SESH_LEN) revert BadSesh();
        if (b[52] != "." || b[53] != "s" || b[54] != "e" || b[55] != "s" || b[56] != "h") revert BadSesh();
        for (uint256 i = 0; i < 52; i++) {
            bytes1 c = b[i];
            // z-base32 alphabet: ybndrfg8ejkmcpqxot1uwisza345h769 (no 0, 2, l, v)
            bool ok = (c >= "a" && c <= "z" && c != "l" && c != "v") || c == "1" || (c >= "3" && c <= "9");
            if (!ok) revert BadSesh();
        }
    }
}
