// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {SecBase} from "./Base.sol";
import {XmrSwapV2} from "../../src/XmrSwapV2.sol";
import {XmrSwapV3} from "../../src/XmrSwapV3.sol";
import {SeshRegistry} from "../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../src/exits/ExitLease.sol";
import {ExitPay, ExitPayForwarder} from "../../src/exits/ExitPay.sol";
import {MockUSDC} from "../../src/MockUSDC.sol";
import {Token, RejectEth} from "../Tokens.sol";

/// Stateless input fuzzing of every external entry point: zero / max values,
/// wrong tokens, overflowing price x hours, malformed names, bad signatures,
/// exact time boundaries.
contract StatelessFuzz is SecBase {
    XmrSwapV3 v3;
    XmrSwapV2 v2;
    SeshRegistry reg;
    ExitLease el;
    ExitPay ep;
    MockUSDC usdc;
    uint256 constant BOB_PK = 0xB0B5EC;
    address bob;
    address alice = address(0xA11CE);
    address seller = address(0x5E11);
    address payout = address(0x9A70);
    uint256 exitId;
    string constant BSESH = "rgwybw99m4d464rbi9z95a7a364n1hauo74ip486ssckf1agw7zo.sesh";
    uint256 constant N = 0xFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFFEBAAEDCE6AF48A03BBFD25E8CD0364141;

    /// Warp past the V3 lock-in window (cancelIntent is refused before it).
    function _li(XmrSwapV3 k, XmrSwapV3.Swap memory s) internal {
        (, uint64 d,) = k.status(k.swapId(s));
        if (block.timestamp < d) vm.warp(d);
    }

    function setUp() public {
        vm.warp(1_000_000);
        (v3, reg, el, usdc) = deployAll();
        v2 = new XmrSwapV2();
        ep = new ExitPay(el, address(v3));
        bob = vm.addr(BOB_PK);
        vm.deal(alice, type(uint128).max);
        vm.deal(seller, 10 ether);
        vm.prank(seller);
        exitId = reg.register{value: SITE_BOND}("exit1.sesh", "exit", "");
    }

    // ------------------------------------------------------------ XmrSwapV3.lock
    function testFuzz_V3_lockParams(
        address claimer, uint256 ck, uint256 rk, uint64 d1, uint64 d2, uint64 grace, uint256 value, uint256 premium
    ) public {
        value = bound(value, 0, 1e30);
        premium = bound(premium, 0, 1e30);
        bool valid = claimer != address(0) && value != 0 && d1 >= 30 minutes && d2 >= 30 minutes
            && grace >= 30 minutes && grace <= 48 hours && ck != rk && ck != 1 && rk != 1
            && uint256(block.timestamp) + d1 + d2 <= type(uint64).max;
        uint256 b0 = address(v3).balance;
        vm.prank(alice);
        try v3.lock{value: value + premium}(claimer, ck, rk, d1, d2, grace, address(0), value, premium, 0) {
            assertTrue(valid, "lock accepted invalid params");
            assertEq(address(v3).balance - b0, value + premium);
        } catch {
            assertTrue(!valid, "lock refused valid params");
        }
    }

    function testFuzz_V3_lockWrongMsgValue(uint256 value, uint256 premium, uint256 sent) public {
        value = bound(value, 1, 1e30);
        premium = bound(premium, 0, 1e30);
        sent = bound(sent, 0, 3e30);
        vm.assume(sent != value + premium);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        v3.lock{value: sent}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, address(0), value, premium, 0);
    }

    function test_V3_lockValuePlusPremiumOverflowReverts() public {
        vm.prank(alice);
        vm.expectRevert(); // checked add
        v3.lock{value: 0}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, address(0), type(uint256).max, 1, 0);
    }

    /// An ERC-20 "asset" with no code, or an EOA, is refused.
    function testFuzz_V3_lockNonContractAsset(address asset) public {
        vm.assume(asset != address(0) && asset.code.length == 0);
        vm.prank(alice);
        vm.expectRevert(XmrSwapV3.BadParams.selector);
        v3.lock(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, asset, 1, 0, 0);
    }

    // ------------------------------------------------------------ secrets / signatures
    function _v3Claiming() internal returns (XmrSwapV3.Swap memory s, bytes32 id) {
        s = XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + 1 hours,
            uint64(block.timestamp) + 2 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.prank(alice);
        id = v3.lock{value: 1 ether}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 1 hours, address(0), 1 ether, 0, 0);
        vm.prank(alice);
        v3.setReady(s);
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, v3.intentDigest(id));
        v3.claimIntent(s, v, r, ss);
    }

    function testFuzz_V3_claimOnlyWithRightSecret(uint256 secret) public {
        vm.assume(secret != BOB_S);
        (XmrSwapV3.Swap memory s,) = _v3Claiming();
        vm.prank(bob);
        vm.expectRevert(XmrSwapV3.BadSecret.selector);
        v3.claim(s, secret, bob);
    }

    function testFuzz_V3_claimOnlyByClaimer(address who) public {
        vm.assume(who != bob);
        (XmrSwapV3.Swap memory s,) = _v3Claiming();
        vm.prank(who);
        vm.expectRevert(XmrSwapV3.NotAllowed.selector);
        v3.claim(s, BOB_S, who);
    }

    /// Relayed claim: any change to recipient, fee or relayer breaks the signature;
    /// the high-s twin of a valid signature is refused.
    function testFuzz_V3_relayedClaimBinding(address to2, uint256 fee2, bool flipS) public {
        (XmrSwapV3.Swap memory s, bytes32 id) = _v3Claiming();
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, v3.claimDigest(id, bob, address(0), 100));
        if (flipS) {
            ss = bytes32(N - uint256(ss));
            v = v == 27 ? 28 : 27;
            vm.expectRevert(XmrSwapV3.BadSignature.selector);
            v3.claimRelayed(s, BOB_S, bob, address(0), 100, v, r, ss);
            return;
        }
        vm.assume(to2 != bob || fee2 != 100);
        fee2 = bound(fee2, 0, 1 ether);
        if (to2 == address(0)) to2 = address(1);
        vm.expectRevert(XmrSwapV3.BadSignature.selector);
        v3.claimRelayed(s, BOB_S, to2, address(0), fee2, v, r, ss);
    }

    function testFuzz_V3_checkSecretRandom(uint256 secret) public view {
        vm.assume(secret != BOB_S);
        assertTrue(!v3.checkSecret(secret, BOB_K));
        assertTrue(!v3.checkSecret(secret + 0, 0));
    }

    /// secret + L (same scalar mod L) is refused: one key share has one encoding.
    function test_V3_secretPlusLRefused() public view {
        uint256 L = 0x1000000000000000000000000000000014def9dea2f79cd65812631a5cf5d3ed;
        assertTrue(v3.checkSecret(BOB_S, BOB_K));
        assertTrue(!v3.checkSecret(BOB_S + L, BOB_K));
    }

    /// Exactly one side may reveal at every second around t1 / t2 / deadline.
    function testFuzz_V3_exclusiveAtBoundaries(uint8 which, int8 off, bool ready, bool cancel) public {
        XmrSwapV3.Swap memory s = XmrSwapV3.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + 1 hours,
            uint64(block.timestamp) + 2 hours, 45 minutes, address(0), 1 ether, 0, 0);
        vm.prank(alice);
        bytes32 id = v3.lock{value: 1 ether}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, 45 minutes, address(0), 1 ether, 0, 0);
        if (cancel) _li(v3, s);
        vm.prank(alice);
        if (cancel) v3.cancelIntent(s);
        else if (ready) v3.setReady(s);
        (, uint64 d,) = v3.status(id);
        uint64 base = which % 3 == 0 ? s.t1 : which % 3 == 1 ? s.t2 : (cancel ? d : s.t2);
        int256 t = int256(uint256(base)) + int256(off % 5);
        if (t < int256(block.timestamp)) t = int256(block.timestamp);
        vm.warp(uint256(t));
        // try the claimer path (intent + reveal in the same second) and the owner path, from a snapshot each
        uint256 snap = vm.snapshotState();
        bool claimer = _tryClaimPath(s, id);
        vm.revertToState(snap);
        vm.prank(alice);
        bool owner;
        try v3.refund(s, ALICE_S) { owner = true; } catch {}
        // The claimer's intent freezes the owner; an owner refund means no claim intent could be accepted.
        assertTrue(!(claimer && owner), "both sides could reveal at the same second");
    }

    function _tryClaimPath(XmrSwapV3.Swap memory s, bytes32 id) internal returns (bool) {
        (uint8 v, bytes32 r, bytes32 ss) = vm.sign(BOB_PK, v3.intentDigest(id));
        try v3.claimIntent(s, v, r, ss) {} catch { return false; }
        vm.prank(bob);
        try v3.claim(s, BOB_S, bob) { return true; } catch { return false; }
    }

    // ------------------------------------------------------------ XmrSwapV2 boundaries
    function testFuzz_V2_boundaries(int8 off, bool ready) public {
        XmrSwapV2.Swap memory s = XmrSwapV2.Swap(alice, bob, BOB_K, ALICE_K, uint64(block.timestamp) + 1 hours,
            uint64(block.timestamp) + 2 hours, address(0), 1 ether, 0);
        vm.prank(alice);
        v2.lock{value: 1 ether}(bob, BOB_K, ALICE_K, 1 hours, 1 hours, address(0), 1 ether, 0);
        if (ready) { vm.prank(alice); v2.setReady(s); }
        uint64 base = off >= 0 ? s.t2 : s.t1;
        int256 t = int256(uint256(base)) + int256(off % 3);
        if (t < int256(block.timestamp)) t = int256(block.timestamp);
        vm.warp(uint256(t));
        uint256 snap = vm.snapshotState();
        vm.prank(bob);
        bool c;
        try v2.claim(s, BOB_S, bob) { c = true; } catch {}
        vm.revertToState(snap);
        vm.prank(alice);
        bool r;
        try v2.refund(s, ALICE_S) { r = true; } catch {}
        assertTrue(!(c && r), "V2: both sides could act at the same second");
    }

    // ------------------------------------------------------------ ExitLease
    function testFuzz_Lease_setOffer(address who, address p, uint128 wph, uint128 uph, uint32 mh) public {
        bool valid = who == seller && p != address(0) && mh != 0 && mh <= 744 && (wph != 0 || uph != 0);
        vm.prank(who);
        try el.setOffer(exitId, p, wph, uph, mh) { assertTrue(valid); } catch { assertTrue(!valid); }
    }

    /// Max price x max hours never overflows (uint128 x uint32 fits) and the quote is exact.
    function testFuzz_Lease_quoteNoOverflow(uint128 wph, uint128 uph, uint32 h) public {
        vm.assume(wph != 0 && uph != 0);
        h = uint32(bound(h, 1, 744));
        vm.prank(seller);
        el.setOffer(exitId, payout, wph, uph, 744);
        assertEq(el.quote(exitId, address(0), h), uint256(wph) * h);
        assertEq(el.quote(exitId, address(usdc), h), uint256(uph) * h);
    }

    function testFuzz_Lease_wrongAssetOrAmount(address asset, uint256 amt, uint256 sent) public {
        vm.prank(seller);
        el.setOffer(exitId, payout, 1e12, 0, 24);
        uint256 q = el.quote(exitId, address(0), 2);
        sent = bound(sent, 0, 1e18);
        amt = bound(amt, 0, 1e18);
        vm.deal(alice, 1e18);
        vm.prank(alice);
        if (asset != address(0)) {
            vm.expectRevert(); // AssetNotAccepted (usdc price 0, or unknown token)
            el.buy{value: 0}(exitId, asset, 2, q, BSESH);
            return;
        }
        vm.assume(amt != q || sent != q);
        vm.expectRevert(ExitLease.WrongAmount.selector);
        el.buy{value: sent}(exitId, address(0), 2, amt, BSESH);
    }

    /// The buyer's .sesh check matches a reference model on random 57-byte strings.
    function testFuzz_Lease_seshCheck(bytes32 a, bytes32 b, uint8 mut) public {
        bytes memory raw = bytes(BSESH);
        bytes memory x = new bytes(57);
        for (uint256 i = 0; i < 57; i++) x[i] = raw[i];
        uint256 pos = uint256(a) % 57;
        x[pos] = bytes1(uint8(uint256(b)) ^ mut);
        bool valid = _modelSesh(x);
        vm.prank(seller);
        el.setOffer(exitId, payout, 1, 0, 1);
        vm.deal(alice, 1);
        vm.prank(alice);
        try el.buy{value: 1}(exitId, address(0), 1, 1, string(x)) { assertTrue(valid); } catch { assertTrue(!valid); }
    }

    function _modelSesh(bytes memory x) internal pure returns (bool) {
        bytes memory alpha = "ybndrfg8ejkmcpqxot1uwisza345h769";
        if (x.length != 57) return false;
        if (x[52] != "." || x[53] != "s" || x[54] != "e" || x[55] != "s" || x[56] != "h") return false;
        for (uint256 i = 0; i < 52; i++) {
            bool ok;
            for (uint256 j = 0; j < 32; j++) if (x[i] == alpha[j]) ok = true;
            if (!ok) return false;
        }
        return true;
    }

    /// Stacking hours: end = max(now, previous end) + hours, never overflows uint64 in practice.
    function testFuzz_Lease_stacking(uint32 h1, uint32 h2, uint32 gap) public {
        h1 = uint32(bound(h1, 1, 744)); h2 = uint32(bound(h2, 1, 744));
        vm.prank(seller);
        el.setOffer(exitId, payout, 1, 0, 744);
        vm.deal(alice, 10_000);
        vm.prank(alice);
        el.buy{value: h1}(exitId, address(0), h1, h1, BSESH);
        uint64 end1 = el.activeUntil(exitId, BSESH);
        vm.warp(block.timestamp + bound(gap, 0, 60 days));
        vm.prank(alice);
        el.buy{value: h2}(exitId, address(0), h2, h2, BSESH);
        uint64 base = end1 > block.timestamp ? end1 : uint64(block.timestamp);
        assertEq(el.activeUntil(exitId, BSESH), base + uint64(h2) * 3600);
    }

    // ------------------------------------------------------------ SeshRegistry
    function testFuzz_Reg_nameCheck(bytes memory label) public {
        vm.assume(label.length <= 70);
        string memory name = string.concat(string(label), ".sesh");
        bool valid = label.length >= 1 && label.length <= 63;
        for (uint256 i = 0; i < label.length && valid; i++) {
            bytes1 ch = label[i];
            valid = (ch >= "a" && ch <= "z") || (ch >= "0" && ch <= "9") || (ch == "-" && i != 0 && i != label.length - 1);
        }
        vm.deal(alice, 1 ether);
        vm.prank(alice);
        try reg.register{value: SITE_BOND}(name, "site", "") { assertTrue(valid); } catch { assertTrue(!valid); }
    }

    function testFuzz_Reg_registerBondAndMeta(uint256 bond, bytes32 tag, uint16 metaLen) public {
        bond = bound(bond, 0, 1 ether);
        metaLen = uint16(bound(metaLen, 0, 400));
        bytes memory meta = new bytes(metaLen);
        bool valid = tag != bytes32(0) && metaLen <= 280 && bond >= (tag == bytes32("maker") ? MAKER_BOND : SITE_BOND);
        vm.deal(alice, 1 ether);
        vm.prank(alice);
        try reg.register{value: bond}("abc.sesh", tag, string(meta)) { assertTrue(valid); } catch { assertTrue(!valid); }
    }

    /// ethValue: huge USDC amounts revert (checked math) instead of wrapping to a small value.
    function testFuzz_Reg_ethValueNoWrap(uint256 v) public view {
        try reg.ethValue(address(usdc), v) returns (uint256 w) {
            // 1 USDC at 3000 USD/ETH = 1e18/3000 wei; result must be monotone and not wrapped
            assertEq(w, (v * 1e8 * 1e18) / 1e6 / 3000e8);
        } catch {
            assertGt(v, type(uint256).max / 1e26); // only on overflow
        }
    }

    function testFuzz_Reg_challengeWrongBond(uint256 amt) public {
        amt = bound(amt, 0, 1 ether);
        vm.assume(amt != CH_BOND);
        vm.deal(alice, 1 ether);
        vm.prank(alice);
        vm.expectRevert(SeshRegistry.BadParams.selector);
        reg.challenge{value: amt}(exitId);
    }

    /// answer at deadline-1 works, at deadline it is refused and resolve works.
    function testFuzz_Reg_challengeBoundary(uint8 off) public {
        vm.deal(alice, 1 ether);
        vm.prank(alice);
        reg.challenge{value: CH_BOND}(exitId);
        uint64 dl = reg.listing(exitId).challengeDeadline;
        uint256 t = uint256(dl) - 2 + (off % 5);
        vm.warp(t);
        uint256 snap = vm.snapshotState();
        bool a;
        vm.prank(seller);
        try reg.answer(exitId) { a = true; } catch {}
        vm.revertToState(snap);
        bool r;
        try reg.resolveChallenge(exitId) { r = true; } catch {}
        assertTrue(a != r, "exactly one of answer / resolve");
        assertEq(a, t < dl);
    }

    // ------------------------------------------------------------ ExitPay
    /// Whatever arrives is either one lease at <= maxCost plus change, or all of it refunded.
    function testFuzz_ExitPay_conservation(uint128 wph, uint32 h, uint256 maxCost, uint256 amount, bool offer) public {
        wph = uint128(bound(wph, 1, 1e16));
        h = uint32(bound(h, 1, 24));
        maxCost = bound(maxCost, 1, 1e18);
        amount = bound(amount, 1, 1e18);
        if (offer) { vm.prank(seller); el.setOffer(exitId, payout, wph, 0, 24); }
        address change = address(0xC4A6);
        ExitPay.Target memory t = ExitPay.Target(address(0), change, exitId, h, maxCost, bytes32(uint256(1)), BSESH);
        address f = ep.deploy(t);
        uint256 p0 = payout.balance;
        vm.deal(address(this), amount);
        (bool ok,) = f.call{value: amount}("");
        assertTrue(ok, "receive never reverts");
        uint256 paid = payout.balance - p0;
        assertEq(address(f).balance, 0);
        assertEq(paid + change.balance, amount);
        if (ExitPayForwarder(payable(f)).done()) {
            assertEq(paid, uint256(wph) * h);
            assertLe(paid, maxCost);
        } else {
            assertEq(paid, 0);
        }
    }

    /// Different targets never share a forwarder address.
    function testFuzz_ExitPay_addressBinding(uint32 h1, uint32 h2, uint256 m1, uint256 m2, bytes32 r1, bytes32 r2) public view {
        vm.assume(h1 != h2 || m1 != m2 || r1 != r2);
        ExitPay.Target memory a = ExitPay.Target(address(0), alice, exitId, h1, m1, r1, BSESH);
        ExitPay.Target memory b = ExitPay.Target(address(0), alice, exitId, h2, m2, r2, BSESH);
        assertTrue(ep.payAddress(a) != ep.payAddress(b));
    }

    /// refundTo == 0 cannot be deployed: money sent to that address is stuck (see report, L-1).
    function test_ExitPay_zeroRefundToIsUndeployable() public {
        ExitPay.Target memory t = ExitPay.Target(address(0), address(0), exitId, 1, 1 ether, bytes32(0), BSESH);
        address f = ep.payAddress(t);
        vm.deal(f, 1 ether); // e.g. a claim paid here
        vm.expectRevert(ExitPay.BadParams.selector);
        ep.settle(t);
        assertEq(f.balance, 1 ether);
    }

    receive() external payable {}
}
