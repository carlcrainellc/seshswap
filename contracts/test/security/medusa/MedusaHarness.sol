// SPDX-License-Identifier: MIT
pragma solidity 0.8.26;
import {XmrSwapV3} from "../../../src/XmrSwapV3.sol";
import {V3TokenHandler} from "../XmrSwapV3Tokens.invariant.t.sol";
import {RegLeaseHandler} from "../RegistryLease.invariant.t.sol";
import {SecBase} from "../Base.sol";
import {SeshRegistry} from "../../../src/registry/SeshRegistry.sol";
import {ExitLease} from "../../../src/exits/ExitLease.sol";
import {MockUSDC} from "../../../src/MockUSDC.sol";

interface IBal { function balanceOf(address) external view returns (uint256); }

/// Medusa harness: the same handler as the Foundry token invariant suite,
/// with the invariants as property_ functions.
///   cd contracts && medusa fuzz --config test/security/medusa/medusa.json
contract MedusaV3 is V3TokenHandler {
    /// payable + funded: under medusa a pranked call with value is paid by the harness.
    constructor() payable V3TokenHandler(new XmrSwapV3()) {}

    function property_solvency() external view returns (bool) {
        for (uint256 i = 0; i < this.nAssets(); i++) {
            address a = assets[i];
            uint256 bal = a == address(0) ? address(c).balance : IBal(a).balanceOf(address(c));
            if (bal != this.obligations(a)) return false;
        }
        return true;
    }

    function property_noTheft() external view returns (bool) {
        return !stolen && !badSecretWorked && !wrongCallerWorked && !feeLockAccepted && !terminalChanged
            && !evil.reentrySucceeded();
    }

    function property_noCancelInLockIn() external view returns (bool) {
        return !cancelInLockIn;
    }
}

/// Registry + ExitLease handler for medusa.
contract MedusaReg is RegLeaseHandler {
    constructor() payable RegLeaseHandler(_d1(), _d2(), _d3(), _d4()) {
        _dep.d.mintTo(taker);
        _dep.d.mintTo(attacker);
    }

    // deploy order matters: the four helpers share one deployment kept in a static slot
    function _d1() private returns (SeshRegistry) { return _deploy().r; }
    function _d2() private returns (ExitLease) { return _dep.e; }
    function _d3() private returns (XmrSwapV3) { return _dep.v; }
    function _d4() private returns (MockUSDC) { return _dep.u; }

    struct Dep { SeshRegistry r; ExitLease e; XmrSwapV3 v; MockUSDC u; Deployer d; }
    Dep private _dep;

    function _deploy() private returns (Dep storage) {
        Deployer d = new Deployer();
        _dep = Dep(d.reg(), d.el(), d.v3(), d.usdc(), d);
        return _dep;
    }

    function property_solvency() external view returns (bool) {
        return address(reg).balance == this.sumBonds() + this.sumOwed() + this.openChallenges() * reg.challengeBond();
    }

    function property_leaseHoldsNothing() external view returns (bool) {
        return address(el).balance == 0 && usdc.balanceOf(address(el)) == 0;
    }

    function property_ownerChecks() external view returns (bool) { return !nonOwnerActed; }
    function property_noEarlyExit() external view returns (bool) { return !withdrewEarly && !bondMoved; }
    function property_challengeBoundary() external view returns (bool) { return !answerAndResolveSameTime; }
    function property_leasePays() external view returns (bool) { return !leaseOnDeadListing && !leaseWrongPay; }
    function property_noReentry() external view returns (bool) { return !reenter.reentered() && !reenter.leaseReentered(); }
}

contract Deployer is SecBase {
    XmrSwapV3 public v3;
    SeshRegistry public reg;
    ExitLease public el;
    MockUSDC public usdc;
    constructor() {
        (v3, reg, el, usdc) = deployAll();
    }
    function mintTo(address a) external { usdc.mint(a, 1e30); }
}
