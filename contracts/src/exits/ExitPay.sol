// SPDX-License-Identifier: MIT
// ExitPay: pay for exit time with the proceeds of a swap claim, in one step.
//
// In an XMR -> ETH swap on XmrSwapV3 the XMR seller is the claimer. Instead of
// claiming to their own wallet they claim to an ExitPay address. That address
// is a CREATE2 forwarder whose constructor arguments ARE the lease target:
// listing, hours, buyer .sesh, the most it may spend, where any change goes,
// and a reference to the swap agreement.
// Nobody can change them after the address is computed:
//   - the claim signature (claimRelayed) fixes `to`, so a relayer can't
//     redirect the funds;
//   - the swap owner (who locked the ETH) never chooses `to` at all;
//   - the forwarder can only buy that one lease or send everything to `refundTo`.
// The swap contract and its refund paths are unchanged.
//
// If the forwarder is deployed before the claim (deploy()), the claim itself
// buys the hours: XmrSwapV3 pushes native coin with all gas, and receive()
// settles. Otherwise anyone may call settle() afterwards. USDC proceeds always
// need settle(). If buying is impossible (offer gone, price above maxCost,
// listing changed hands) everything goes to refundTo.
//
// Testnet only. Not audited.
pragma solidity 0.8.26;

import {ExitLease} from "./ExitLease.sol";

interface IERC20Min {
    function balanceOf(address) external view returns (uint256);
    function approve(address, uint256) external returns (bool);
    function transfer(address, uint256) external returns (bool);
}

interface ISwapWithdraw {
    function owed(address asset, address who) external view returns (uint256);
    function withdraw(address asset, address to) external;
}

/// One lease target. All fields are fixed at construction.
contract ExitPayForwarder {
    ExitLease public immutable lease;
    address public immutable swap;
    address public immutable asset; // address(0) = ETH, else the ExitLease USDC
    address public immutable refundTo;
    uint256 public immutable listingId;
    uint256 public immutable maxCost;
    uint32 public immutable hoursBought;
    bytes32 public immutable ref; // the swap agreement this pays for
    string public buyerSesh;

    bool public done; // a lease was bought
    uint256 public leaseId;
    bool private _busy;

    event Bought(uint256 indexed leaseId, uint256 cost, uint256 change);
    event Refunded(uint256 amount, string why);

    constructor(
        ExitLease lease_,
        address swap_,
        address asset_,
        address refundTo_,
        uint256 listingId_,
        uint32 hours_,
        uint256 maxCost_,
        bytes32 ref_,
        string memory buyerSesh_
    ) {
        lease = lease_;
        swap = swap_;
        asset = asset_;
        refundTo = refundTo_;
        listingId = listingId_;
        hoursBought = hours_;
        maxCost = maxCost_;
        ref = ref_;
        buyerSesh = buyerSesh_;
    }

    /// The claim pays in here; settle on the spot. Never reverts on a failed
    /// buy (the claim must not fail): the money then goes to refundTo, or stays
    /// for settle() if even that transfer fails.
    receive() external payable {
        if (asset == address(0) && !_busy) _settle();
    }

    /// Anyone: collect a deferred payout from the swap contract (if any) and
    /// buy the lease, or refund.
    function settle() external {
        if (_busy) return;
        if (swap != address(0)) {
            try ISwapWithdraw(swap).owed(asset, address(this)) returns (uint256 o) {
                if (o > 0) {
                    _busy = true; // the withdraw pushes ETH into receive()
                    try ISwapWithdraw(swap).withdraw(asset, address(this)) {} catch {}
                    _busy = false;
                }
            } catch {}
        }
        _settle();
    }

    function _balance() private view returns (uint256) {
        return asset == address(0) ? address(this).balance : IERC20Min(asset).balanceOf(address(this));
    }

    function _settle() private {
        uint256 bal = _balance();
        if (bal == 0) return;
        _busy = true;
        if (!done) {
            (bool ok, uint256 cost) = _tryBuy(bal);
            if (ok) {
                done = true;
                emit Bought(leaseId, cost, bal - cost);
                bal -= cost;
            }
        }
        if (bal > 0) {
            _sendRefund(bal, done ? "change" : "lease not bought");
        }
        _busy = false;
    }

    function _tryBuy(uint256 bal) private returns (bool, uint256) {
        uint256 cost;
        try lease.quote(listingId, asset, hoursBought) returns (uint256 q) {
            cost = q;
        } catch {
            return (false, 0);
        }
        if (cost > maxCost || cost > bal) return (false, 0);
        if (asset == address(0)) {
            try lease.buy{value: cost}(listingId, asset, hoursBought, cost, buyerSesh) returns (uint256 id) {
                leaseId = id;
                return (true, cost);
            } catch {
                return (false, 0);
            }
        }
        IERC20Min(asset).approve(address(lease), cost);
        try lease.buy(listingId, asset, hoursBought, cost, buyerSesh) returns (uint256 id) {
            leaseId = id;
            return (true, cost);
        } catch {
            IERC20Min(asset).approve(address(lease), 0);
            return (false, 0);
        }
    }

    function _sendRefund(uint256 amt, string memory why) private {
        bool ok;
        if (asset == address(0)) {
            (ok,) = refundTo.call{value: amt}("");
        } else {
            try IERC20Min(asset).transfer(refundTo, amt) returns (bool r) {
                ok = r;
            } catch {}
        }
        if (ok) emit Refunded(amt, why);
    }
}

/// Computes and deploys ExitPay forwarders. No owner, no fees, no state.
contract ExitPay {
    ExitLease public immutable lease;
    address public immutable swap;

    event Deployed(address indexed forwarder, uint256 indexed listingId, string buyerSesh, address refundTo);

    error BadParams();

    constructor(ExitLease lease_, address swap_) {
        lease = lease_;
        swap = swap_;
    }

    struct Target {
        address asset;
        address refundTo;
        uint256 listingId;
        uint32 hoursBought;
        uint256 maxCost;
        bytes32 ref; // identifies the swap agreement (e.g. its terms hash), so each swap gets its own address
        string buyerSesh;
    }

    function _initCode(Target calldata t) private view returns (bytes memory) {
        return abi.encodePacked(
            type(ExitPayForwarder).creationCode,
            abi.encode(lease, swap, t.asset, t.refundTo, t.listingId, t.hoursBought, t.maxCost, t.ref, t.buyerSesh)
        );
    }

    /// The address a claim should pay to for this target.
    function payAddress(Target calldata t) public view returns (address) {
        bytes32 h = keccak256(abi.encodePacked(bytes1(0xff), address(this), bytes32(0), keccak256(_initCode(t))));
        return address(uint160(uint256(h)));
    }

    /// Deploy the forwarder (idempotent). Do this before the claim and the
    /// claim itself buys the hours.
    function deploy(Target calldata t) public returns (address f) {
        if (t.refundTo == address(0) || t.hoursBought == 0 || t.maxCost == 0) revert BadParams();
        f = payAddress(t);
        if (f.code.length > 0) return f;
        bytes memory code = _initCode(t);
        assembly ("memory-safe") {
            f := create2(0, add(code, 0x20), mload(code), 0)
        }
        if (f == address(0)) revert BadParams();
        emit Deployed(f, t.listingId, t.buyerSesh, t.refundTo);
        // Money that arrived before the code: settle it now.
        ExitPayForwarder(payable(f)).settle();
    }

    /// Deploy if needed and settle.
    function settle(Target calldata t) external returns (address f) {
        f = payAddress(t);
        if (f.code.length == 0) return deploy(t);
        ExitPayForwarder(payable(f)).settle();
    }
}
